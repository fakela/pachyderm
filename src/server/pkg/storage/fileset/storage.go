package fileset

import (
	"context"
	"fmt"
	"io"
	"math"
	"path"
	"strings"

	units "github.com/docker/go-units"
	"github.com/pachyderm/pachyderm/src/client/pkg/errors"
	"github.com/pachyderm/pachyderm/src/server/pkg/obj"
	"github.com/pachyderm/pachyderm/src/server/pkg/storage/chunk"
	"github.com/pachyderm/pachyderm/src/server/pkg/storage/fileset/index"
	"golang.org/x/sync/semaphore"
)

const (
	prefix = "pfs"
	// TODO Not sure if these are the tags we should use, but the header and padding tag should show up before and after respectively in the
	// lexicographical ordering of file content tags.
	// headerTag is the tag used for the tar header bytes.
	headerTag = ""
	// paddingTag is the tag used for the padding bytes at the end of a tar entry.
	paddingTag = "~"
	// DefaultMemoryThreshold is the default for the memory threshold that must
	// be met before a file set part is serialized (excluding close).
	DefaultMemoryThreshold = 1024 * units.MB
	// DefaultShardThreshold is the default for the size threshold that must
	// be met before a shard is created by the shard function.
	DefaultShardThreshold = 1024 * units.MB
	// DefaultLevelZeroSize is the default size for level zero in the compacted
	// representation of a file set.
	DefaultLevelZeroSize = 1 * units.MB
	// DefaultLevelSizeBase is the default base of the exponential growth function
	// for level sizes in the compacted representation of a file set.
	DefaultLevelSizeBase = 10
	// Diff is the suffix of a path that points to the diff of the prefix.
	Diff = "diff"
	// Compacted is the suffix of a path that points to the compaction of the prefix.
	Compacted = "compacted"
)

// Storage is the abstraction that manages fileset storage.
type Storage struct {
	objC                         obj.Client
	chunks                       *chunk.Storage
	memThreshold, shardThreshold int64
	levelZeroSize                int64
	levelSizeBase                int
	filesetSem                   *semaphore.Weighted
}

// NewStorage creates a new Storage.
func NewStorage(objC obj.Client, chunks *chunk.Storage, opts ...StorageOption) *Storage {
	s := &Storage{
		objC:           objC,
		chunks:         chunks,
		memThreshold:   DefaultMemoryThreshold,
		shardThreshold: DefaultShardThreshold,
		levelZeroSize:  DefaultLevelZeroSize,
		levelSizeBase:  DefaultLevelSizeBase,
		filesetSem:     semaphore.NewWeighted(math.MaxInt64),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ChunkStorage returns the underlying chunk storage instance for this storage instance.
func (s *Storage) ChunkStorage() *chunk.Storage {
	return s.chunks
}

// New creates a new in-memory fileset.
func (s *Storage) New(ctx context.Context, fileSet, defaultTag string, opts ...Option) (*FileSet, error) {
	fileSet = applyPrefix(fileSet)
	return newFileSet(ctx, s, fileSet, s.memThreshold, defaultTag, opts...)
}

// NewWriter makes a Writer backed by the path `fileSet` in object storage.
func (s *Storage) NewWriter(ctx context.Context, fileSet string, opts ...WriterOption) *Writer {
	fileSet = applyPrefix(fileSet)
	return s.newWriter(ctx, fileSet, opts...)
}

// NewReader makes a Reader backed by the path `fileSet` in object storage.
func (s *Storage) NewReader(ctx context.Context, fileSet string, opts ...index.Option) *Reader {
	fileSet = applyPrefix(fileSet)
	return s.newReader(ctx, fileSet, opts...)
}

func (s *Storage) newWriter(ctx context.Context, fileSet string, opts ...WriterOption) *Writer {
	return newWriter(ctx, s.objC, s.chunks, fileSet, opts...)
}

// TODO Expose some notion of read ahead (read a certain number of chunks in parallel).
// this will be necessary to speed up reading large files.
func (s *Storage) newReader(ctx context.Context, fileSet string, opts ...index.Option) *Reader {
	return newReader(ctx, s.objC, s.chunks, fileSet, opts...)
}

// NewMergeReader returns a merge reader for a set for filesets.
func (s *Storage) NewMergeReader(ctx context.Context, fileSets []string, opts ...index.Option) (*MergeReader, error) {
	fileSets = applyPrefixes(fileSets)
	return s.newMergeReader(ctx, fileSets, opts...)
}

func (s *Storage) newMergeReader(ctx context.Context, fileSets []string, opts ...index.Option) (*MergeReader, error) {
	var rs []*Reader
	for _, fileSet := range fileSets {
		if err := s.objC.Walk(ctx, fileSet, func(name string) error {
			rs = append(rs, s.newReader(ctx, name, opts...))
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return newMergeReader(rs), nil
}

// ResolveIndexes resolves index entries that are spread across multiple filesets.
func (s *Storage) ResolveIndexes(ctx context.Context, fileSets []string, f func(*index.Index) error, opts ...index.Option) error {
	fileSets = applyPrefixes(fileSets)
	mr, err := s.newMergeReader(ctx, fileSets, opts...)
	if err != nil {
		return err
	}
	w := s.newWriter(ctx, "", WithNoUpload(f))
	if err := mr.WriteTo(w); err != nil {
		return err
	}
	return w.Close()
}

// Shard shards the merge of the file sets with the passed in prefix into file ranges.
// TODO This should be extended to be more configurable (different criteria
// for creating shards).
func (s *Storage) Shard(ctx context.Context, fileSets []string, shardFunc ShardFunc) error {
	mr, err := s.NewMergeReader(ctx, fileSets)
	if err != nil {
		return err
	}
	return shard(mr, s.shardThreshold, shardFunc)
}

// Compact compacts a set of filesets into an output fileset.
func (s *Storage) Compact(ctx context.Context, outputFileSet string, inputFileSets []string, opts ...index.Option) error {
	outputFileSet = applyPrefix(outputFileSet)
	inputFileSets = applyPrefixes(inputFileSets)
	w := s.newWriter(ctx, outputFileSet)
	mr, err := s.newMergeReader(ctx, inputFileSets, opts...)
	if err != nil {
		return err
	}
	if err := mr.WriteTo(w); err != nil {
		return err
	}
	return w.Close()
}

// CompactSpec specifies the input and output for a compaction operation.
type CompactSpec struct {
	Output string
	Input  []string
}

// CompactSpec returns a compaction specification that determines the input filesets (the diff file set and potentially
// compacted filesets) and output fileset.
func (s *Storage) CompactSpec(ctx context.Context, fileSet string, compactedFileSet ...string) (ret *CompactSpec, retErr error) {
	if len(compactedFileSet) > 1 {
		return nil, errors.WithStack(errors.Errorf("multiple compacted FileSets"))
	}
	// internal vs external path transforms
	fileSet = applyPrefix(fileSet)
	compactedFileSet = applyPrefixes(compactedFileSet)
	defer func() {
		if ret != nil {
			ret.Input = removePrefixes(ret.Input)
			ret.Output = removePrefix(ret.Output)
			fmt.Println(ret.Input, ret.Output)
		}
	}()
	idx, err := index.GetTopLevelIndex(ctx, s.objC, path.Join(fileSet, Diff))
	if err != nil {
		return nil, err
	}
	size := idx.SizeBytes
	spec := &CompactSpec{
		Input: []string{path.Join(fileSet, Diff)},
	}
	level := 0
	// Handle first commit being compacted.
	if len(compactedFileSet) == 0 {
		for size > s.levelSize(level) {
			level++
		}
		spec.Output = path.Join(fileSet, Compacted, levelName(level))
		return spec, nil
	}
	// while we can't fit it all in the current level
	for size > s.levelSize(level) {
		levelPath := path.Join(compactedFileSet[0], Compacted, levelName(level))
		idx, err := index.GetTopLevelIndex(ctx, s.objC, levelPath)
		if err != nil && !s.objC.IsNotExist(err) {
			return nil, err
		}
		// the level does exist. Add it's size to the total, and mark it for compaction.
		if err == nil {
			spec.Input = append(spec.Input, levelPath)
			size += idx.SizeBytes
		}
		level++
	}
	// now we know the output level
	spec.Output = path.Join(fileSet, Compacted, levelName(level))
	// copy the other levels that may exist
	for i := level + 1; ; i++ {
		levelPath := path.Join(compactedFileSet[0], Compacted, levelName(i))
		_, err := index.GetTopLevelIndex(ctx, s.objC, levelPath)
		if err != nil {
			if s.objC.IsNotExist(err) {
				break
			}
			return nil, err
		}
		dst := path.Join(fileSet, Compacted, levelName(i))
		if err := copyObject(ctx, s.objC, levelPath, dst); err != nil {
			return nil, err
		}
	}
	return spec, nil
}

// Delete deletes a fileset.
func (s *Storage) Delete(ctx context.Context, fileSet string) error {
	fileSet = applyPrefix(fileSet)
	return s.objC.Walk(ctx, fileSet, func(name string) error {
		if err := s.chunks.DeleteSemanticReference(ctx, name); err != nil {
			return err
		}
		return s.objC.Delete(ctx, name)
	})
}

// WalkFileSet calls f with the path of every primitive fileSet under prefix.
func (s *Storage) WalkFileSet(ctx context.Context, prefix string, f func(string) error) error {
	return s.objC.Walk(ctx, applyPrefix(prefix), func(p string) error {
		return f(removePrefix(p))
	})
}

func (s *Storage) levelSize(i int) int64 {
	return s.levelZeroSize * int64(math.Pow(float64(s.levelSizeBase), float64(i)))
}

func applyPrefix(fileSet string) string {
	fileSet = strings.TrimLeft(fileSet, "/")
	if strings.HasPrefix(fileSet, prefix) {
		return fileSet
	}
	return path.Join(prefix, fileSet)
}

func applyPrefixes(fileSets []string) []string {
	var prefixedFileSets []string
	for _, fileSet := range fileSets {
		prefixedFileSets = append(prefixedFileSets, applyPrefix(fileSet))
	}
	return prefixedFileSets
}

func removePrefix(fileSet string) string {
	if !strings.HasPrefix(fileSet, prefix) {
		panic(fileSet + " does not have prefix " + prefix)
	}
	return fileSet[len(prefix):]
}

func removePrefixes(xs []string) (ys []string) {
	for i := range xs {
		ys = append(ys, removePrefix(xs[i]))
	}
	return ys
}

// SubFileSetStr returns the string representation of a subfileset.
func SubFileSetStr(subFileSet int64) string {
	return fmt.Sprintf("%020d", subFileSet)
}

func levelName(i int) string {
	return fmt.Sprintf("%020d", i)
}

func copyObject(ctx context.Context, objC obj.Client, src, dst string) error {
	w, err := objC.Writer(ctx, dst)
	if err != nil {
		return err
	}
	defer w.Close()
	r, err := objC.Reader(ctx, src, 0, 0)
	if err != nil {
		return err
	}
	defer r.Close()
	if _, err := io.Copy(w, r); err != nil {
		return err
	}
	return w.Close()
}
