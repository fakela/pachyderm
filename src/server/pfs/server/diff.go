package server

import (
	"github.com/pachyderm/pachyderm/src/client/pfs"
	"github.com/pachyderm/pachyderm/src/server/pkg/storage/fileset"
	"golang.org/x/net/context"
	"golang.org/x/sync/errgroup"
)

// Differ compares two sources and iterates over the items that are not equal.
type Differ struct {
	a, b Source
}

func NewDiffer(a, b Source) *Differ {
	return &Differ{a: a, b: b}
}

func (d *Differ) IterateDiff(ctx context.Context, cb func(aFi, bFi *pfs.FileInfoV2) error) error {
	ctx, cf := context.WithCancel(ctx)
	defer cf()
	aInfos := make(chan *pfs.FileInfoV2)
	bInfos := make(chan *pfs.FileInfoV2)
	eg, ctx := errgroup.WithContext(ctx)
	// iterate over a
	eg.Go(func() error {
		defer close(aInfos)
		return d.a.Iterate(ctx, func(fi *pfs.FileInfoV2, _ fileset.File) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case aInfos <- fi:
				return nil
			}
		})
	})
	// iterate over b
	eg.Go(func() error {
		defer close(bInfos)
		return d.a.Iterate(ctx, func(fi *pfs.FileInfoV2, _ fileset.File) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case bInfos <- fi:
				return nil
			}
		})
	})
	eg.Go(func() error {
		aFi, aOpen := <-aInfos
		bFi, bOpen := <-bInfos
		for aOpen && bOpen {
			switch {
			case aFi.File.Path < bFi.File.Path:
				if err := cb(aFi, nil); err != nil {
					return err
				}
				aFi, aOpen = <-aInfos
			case bFi.File.Path < aFi.File.Path:
				if err := cb(nil, bFi); err != nil {
					return err
				}
				bFi, bOpen = <-bInfos
			default:
				if !equalFileInfos(aFi, bFi) {
					if err := cb(aFi, bFi); err != nil {
						return err
					}
				}
				aFi, aOpen = <-aInfos
				bFi, bOpen = <-bInfos
			}
		}
		for aFi = range aInfos {
			if err := cb(aFi, nil); err != nil {
				return err
			}
		}
		for bFi = range bInfos {
			if err := cb(nil, bFi); err != nil {
				return err
			}
		}
		return nil
	})
	return eg.Wait()
}

func equalFileInfos(aFi, bFi *pfs.FileInfoV2) bool {
	return aFi.Hash == bFi.Hash
}

// type sourceIter struct {
// 	peek     fileset.File
// 	fileChan chan fileset.File
// 	errChan  chan error
// }

// func newSourceIter(ctx context.Context, source Source) *sourceIter {
// 	fileChan := make(chan fileset.File)
// 	errChan := make(chan error, 1)
// 	go func() {
// 		if err := source.Iterate(ctx, func(file fileset.File) error {
// 			fileChan <- file
// 			return nil
// 		}); err != nil {
// 			errChan <- err
// 			return
// 		}
// 		close(fileChan)
// 	}()
// 	return &stream{
// 		fileChan: fileChan,
// 		errChan:  errChan,
// 	}
// }

// func (s *stream) Peek() (fileset.File, error) {
// 	if s.peek != nil {
// 		return s.peek, nil
// 	}
// 	var err error
// 	s.peek, err = s.Next()
// 	return s.peek, err
// }

// func (s *stream) Next() (fileset.File, error) {
// 	if s.peek != nil {
// 		tmp := s.peek
// 		s.peek = nil
// 		return tmp, nil
// 	}
// 	select {
// 	case file, more := <-s.fileChan:
// 		if !more {
// 			return nil, io.EOF
// 		}
// 		return file, nil
// 	case err := <-s.errChan:
// 		return nil, err
// 	}
// }
