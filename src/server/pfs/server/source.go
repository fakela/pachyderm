package server

import (
	"hash"
	"io"
	"strings"

	"github.com/pachyderm/pachyderm/src/client"
	"github.com/pachyderm/pachyderm/src/client/pfs"
	"github.com/pachyderm/pachyderm/src/client/pkg/errors"
	"github.com/pachyderm/pachyderm/src/server/pkg/storage/fileset"
	"github.com/pachyderm/pachyderm/src/server/pkg/storage/fileset/index"
	"github.com/pachyderm/pachyderm/src/server/pkg/storage/fileset/tar"
	"golang.org/x/net/context"
)

// FileReader is a PFS wrapper for a fileset.MergeReader.
// The primary purpose of this abstraction is to convert from index.Index to
// pfs.FileInfo and to convert a set of index hashes to a file hash.
type FileReader struct {
	file      *pfs.File
	idx       *index.Index
	fmr       *fileset.FileMergeReader
	mr        *fileset.MergeReader
	fileCount int
	hash      hash.Hash
}

func newFileReader(file *pfs.File, idx *index.Index, fmr *fileset.FileMergeReader, mr *fileset.MergeReader) *FileReader {
	h := pfs.NewHash()
	for _, dataRef := range idx.DataOp.DataRefs {
		// TODO Pull from chunk hash.
		h.Write([]byte(dataRef.Hash))
	}
	return &FileReader{
		file: file,
		idx:  idx,
		fmr:  fmr,
		mr:   mr,
		hash: h,
	}
}

func (fr *FileReader) updateFileInfo(idx *index.Index) {
	fr.fileCount++
	for _, dataRef := range idx.DataOp.DataRefs {
		fr.hash.Write([]byte(dataRef.Hash))
	}
}

// Info returns the info for the file.
func (fr *FileReader) Info() *pfs.FileInfoV2 {
	return &pfs.FileInfoV2{
		File: fr.file,
		Hash: pfs.EncodeHash(fr.hash.Sum(nil)),
	}
}

// Get writes a tar stream that contains the file.
func (fr *FileReader) Get(w io.Writer, noPadding ...bool) error {
	if err := fr.fmr.Get(w); err != nil {
		return err
	}
	for fr.fileCount > 0 {
		fmr, err := fr.mr.Next()
		if err != nil {
			return err
		}
		if err := fmr.Get(w); err != nil {
			return err
		}
		fr.fileCount--
	}
	if len(noPadding) > 0 && noPadding[0] {
		return nil
	}
	// Close a tar writer to create tar EOF padding.
	return tar.NewWriter(w).Close()
}

func (fr *FileReader) drain() error {
	for fr.fileCount > 0 {
		if _, err := fr.mr.Next(); err != nil {
			return err
		}
		fr.fileCount--
	}
	return nil
}

// Source iterates over FileInfoV2s generated from a fileset.Source
type Source struct {
	commit        *pfs.Commit
	getReader     func() fileset.FileSource
	computeHashes bool
}

// NewSource creates a Source which emits FileInfoV2s with the information from commit, and the entries from readers
// returned by getReader.  If getReader returns different Readers all bets are off.
func NewSource(commit *pfs.Commit, computeHashes bool, getReader func() fileset.FileSource) *Source {
	return &Source{
		commit:        commit,
		getReader:     getReader,
		computeHashes: computeHashes,
	}
}

// Iterate calls cb for each File in the underlying fileset.FileSource, with a FileInfoV2 computed
// during iteration, and the File.
func (s *Source) Iterate(ctx context.Context, cb func(*pfs.FileInfoV2, fileset.File) error) error {
	ctx, cf := context.WithCancel(ctx)
	defer cf()
	fs1 := s.getReader()
	fs2 := s.getReader()
	s2 := newStream(ctx, fs2)
	cache := make(map[string][]byte)
	return fs1.Iterate(ctx, func(fr fileset.File) error {
		idx := fr.Index()
		fi := &pfs.FileInfoV2{
			File: client.NewFile(s.commit.Repo.Name, s.commit.ID, idx.Path),
		}
		if s.computeHashes {
			var err error
			var hashBytes []byte
			if indexIsDir(idx) {
				hashBytes, err = computeHash(cache, s2, idx.Path)
				if err != nil {
					return err
				}
			} else {
				hashBytes = computeFileHash(idx)
			}
			fi.Hash = pfs.EncodeHash(hashBytes)
		}
		if err := cb(fi, fr); err != nil {
			return err
		}
		delete(cache, idx.Path)
		return nil
	})
}

func computeHash(cache map[string][]byte, s *stream, target string) ([]byte, error) {
	if hashBytes, exists := cache[target]; exists {
		return hashBytes, nil
	}
	// consume the target from the stream
	fr, err := s.Next()
	if err != nil {
		if err == io.EOF {
			return nil, errors.Errorf("stream is done, can't compute hash for %s", target)
		}
		return nil, err
	}
	idx := fr.Index()
	if idx.Path != target {
		return nil, errors.Errorf("stream is wrong place to compute hash for %s", target)
	}
	// for file
	if !indexIsDir(idx) {
		return computeFileHash(idx), nil
	}
	// for directory
	h := pfs.NewHash()
	for {
		f2, err := s.Peek()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		idx2 := f2.Index()
		if !strings.HasPrefix(idx2.Path, target) {
			break
		}
		childHash, err := computeHash(cache, s, idx2.Path)
		if err != nil {
			return nil, err
		}
		h.Write(childHash)
	}
	hashBytes := h.Sum(nil)
	cache[target] = hashBytes
	return hashBytes, nil
}

func computeFileHash(idx *index.Index) []byte {
	h := pfs.NewHash()
	if idx.DataOp != nil {
		for _, dataRef := range idx.DataOp.DataRefs {
			if dataRef.Hash == "" {
				h.Write([]byte(dataRef.ChunkInfo.Chunk.Hash))
			} else {
				h.Write([]byte(dataRef.Hash))
			}
		}
	}
	return h.Sum(nil)
}

type stream struct {
	peek     fileset.File
	fileChan chan fileset.File
	errChan  chan error
}

func newStream(ctx context.Context, source fileset.FileSource) *stream {
	fileChan := make(chan fileset.File)
	errChan := make(chan error, 1)
	go func() {
		if err := source.Iterate(ctx, func(file fileset.File) error {
			fileChan <- file
			return nil
		}); err != nil {
			errChan <- err
			return
		}
		close(fileChan)
	}()
	return &stream{
		fileChan: fileChan,
		errChan:  errChan,
	}
}

func (s *stream) Peek() (fileset.File, error) {
	if s.peek != nil {
		return s.peek, nil
	}
	var err error
	s.peek, err = s.Next()
	return s.peek, err
}

func (s *stream) Next() (fileset.File, error) {
	if s.peek != nil {
		tmp := s.peek
		s.peek = nil
		return tmp, nil
	}
	select {
	case file, more := <-s.fileChan:
		if !more {
			return nil, io.EOF
		}
		return file, nil
	case err := <-s.errChan:
		return nil, err
	}
}

func indexIsDir(idx *index.Index) bool {
	return strings.HasSuffix(idx.Path, "/")
}
