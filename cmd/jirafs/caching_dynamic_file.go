package main

import (
	"github.com/knusbaum/go9p/fs"
	"github.com/knusbaum/go9p/proto"
)

// CachingDynamicFile is a File implementation that will serve dynamic content. The first time a
// client reads from the CachingDynamicFile, content is generated for the fid with the function
// genContent, which is passed to NewDynamicFile. Subsequent reads on the fid will return byte
// ranges from that content generated when the fid was opened. Closing the fid releases the
// content.
//
// This differes from fs.DynamicFile in that the content is not generated on Open() but on the
// first Read().
type CachingDynamicFile struct {
	fs.BaseFile
	fidContent map[uint64][]byte
	genContent func() []byte
}

// NewDynamicFile creates a new DynamicFile that will use getContent to
// generate the file's content for each fid that opens it.
func NewCachingDynamicFile(s *proto.Stat, genContent func() []byte) *CachingDynamicFile {
	return &CachingDynamicFile{
		BaseFile:   *fs.NewBaseFile(s),
		fidContent: make(map[uint64][]byte),
		genContent: genContent,
	}
}

func (f *CachingDynamicFile) Open(fid uint64, omode proto.Mode) error {
	// 	f.Lock()
	// 	defer f.Unlock()
	// 	f.fidContent[fid] = f.genContent()
	// 	return nil
	return nil
}

func (f *CachingDynamicFile) getContent(fid uint64) (bs []byte, ok bool) {
	f.RLock()
	defer f.RUnlock()
	data, ok := f.fidContent[fid]
	return data, ok
}

func (f *CachingDynamicFile) generate(fid uint64) []byte {
	f.Lock()
	defer f.Unlock()
	data := f.genContent()
	f.fidContent[fid] = data
	return data
}

func (f *CachingDynamicFile) Read(fid uint64, offset uint64, count uint64) ([]byte, error) {
	data, ok := f.getContent(fid)
	if !ok {
		data = f.generate(fid)
	}

	flen := uint64(len(data))
	if offset >= flen {
		return []byte{}, nil
	}
	if offset+count > flen {
		count = flen - offset
	}
	return data[offset : offset+count], nil
}

func (f *CachingDynamicFile) Close(fid uint64) error {
	delete(f.fidContent, fid)
	return nil
}
