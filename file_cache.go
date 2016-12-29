package cacheddownloader

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"code.cloudfoundry.org/archiver/compressor"
)

var (
	lock          = &sync.Mutex{}
	EntryNotFound = errors.New("Entry Not Found")
	AlreadyClosed = errors.New("Already closed directory")
	NotCacheable  = errors.New("Not cacheable directory")
)

type FileCache struct {
	CachedPath     string
	maxSizeInBytes int64
	Entries        map[string]*FileCacheEntry
	OldEntries     map[string]*FileCacheEntry
	Seq            uint64
}

type FileCacheEntry struct {
	Size                  int64
	Access                time.Time
	CachingInfo           CachingInfoType
	FilePath              string
	ExpandedDirectoryPath string
	directoryInUseCount   int
	fileInUseCount        int
}

func NewCache(dir string, maxSizeInBytes int64) *FileCache {
	return &FileCache{
		CachedPath:     dir,
		maxSizeInBytes: maxSizeInBytes,
		Entries:        map[string]*FileCacheEntry{},
		OldEntries:     map[string]*FileCacheEntry{},
		Seq:            0,
	}
}

func newFileCacheEntry(cachePath string, size int64, cachingInfo CachingInfoType) *FileCacheEntry {
	return &FileCacheEntry{
		Size:                  size,
		FilePath:              cachePath,
		Access:                time.Now(),
		CachingInfo:           cachingInfo,
		ExpandedDirectoryPath: "",
	}
}

func (e *FileCacheEntry) inUse() bool {
	return e.directoryInUseCount > 0 || e.fileInUseCount > 0
}

func (e *FileCacheEntry) decrementUse() {
	e.decrementFileInUseCount()
	e.decrementDirectoryInUseCount()
}

func (e *FileCacheEntry) incrementDirectoryInUseCount() {
	e.directoryInUseCount++
}

func (e *FileCacheEntry) decrementDirectoryInUseCount() {
	e.directoryInUseCount--

	// Delete the directory if the tarball is the only asset
	// being used or if the directory has been removed (in use count -1)
	if e.directoryInUseCount < 0 || (e.directoryInUseCount == 0 && e.fileInUseCount > 0) {
		err := os.RemoveAll(e.ExpandedDirectoryPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Unable to delete cached directory", err)
		}
		e.ExpandedDirectoryPath = ""

		if e.fileInUseCount > 0 {
			e.Size = e.Size / 2
		}
	}
}

func (e *FileCacheEntry) incrementFileInUseCount() {
	e.fileInUseCount++
}

func (e *FileCacheEntry) decrementFileInUseCount() {
	e.fileInUseCount--

	// Delete the file if the file is not being used and there is
	// a directory of if the file has been removed (in use count -1)
	if e.fileInUseCount < 0 || (e.fileInUseCount == 0 && e.directoryInUseCount > 0) {
		err := os.RemoveAll(e.FilePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Unable to delete cached file", err)
		}

		if e.directoryInUseCount > 0 {
			e.Size = e.Size / 2
		}
	}
}

func (e *FileCacheEntry) fileDoesNotExist() bool {
	_, err := os.Stat(e.FilePath)
	return os.IsNotExist(err)
}

func (e *FileCacheEntry) dirDoesNotExist() bool {
	if e.ExpandedDirectoryPath == "" {
		return true
	}
	_, err := os.Stat(e.ExpandedDirectoryPath)
	return os.IsNotExist(err)
}

// Can we change this to be an io.ReadCloser return
func (e *FileCacheEntry) readCloser() (*CachedFile, error) {
	var f *os.File
	var err error

	if e.fileDoesNotExist() {
		f, err = os.Create(e.FilePath)
		if err != nil {
			return nil, err
		}

		err = compressor.WriteTar(e.ExpandedDirectoryPath+"/", f)
		if err != nil {
			return nil, err
		}

		// If the directory is not used remove it
		if e.directoryInUseCount == 0 {
			err = os.RemoveAll(e.ExpandedDirectoryPath)
			if err != nil {
				fmt.Fprintln(os.Stderr, "Unable to remove cached directory", err)
			}
		} else {
			// Double the size to account for both assets
			e.Size = e.Size * 2
		}
	} else {
		f, err = os.Open(e.FilePath)
		if err != nil {
			return nil, err
		}
	}

	e.incrementFileInUseCount()

	readCloser := NewFileCloser(f, func(filePath string) {
		lock.Lock()
		e.decrementFileInUseCount()
		lock.Unlock()
	})

	return readCloser, nil
}

func (e *FileCacheEntry) expandedDirectory() (string, error) {
	// if it has not been extracted before expand it!
	if e.dirDoesNotExist() {
		e.ExpandedDirectoryPath = e.FilePath + ".d"
		err := extractTarToDirectory(e.FilePath, e.ExpandedDirectoryPath)
		if err != nil {
			return "", err
		}

		// If the file is not in use, we can delete it
		if e.fileInUseCount == 0 {
			err = os.RemoveAll(e.FilePath)
			if err != nil {
				fmt.Fprintln(os.Stderr, "Unable to delete the cached file", err)
			}
		}
	}

	e.incrementDirectoryInUseCount()

	return e.ExpandedDirectoryPath, nil
}

func (c *FileCache) CloseDirectory(cacheKey, dirPath string) error {
	lock.Lock()
	defer lock.Unlock()

	entry := c.Entries[cacheKey]
	if entry != nil && entry.ExpandedDirectoryPath == dirPath {
		if entry.directoryInUseCount == 0 {
			// We don't think anybody is using this so throw an error
			return AlreadyClosed
		}

		entry.decrementDirectoryInUseCount()
		return nil
	}

	// Key didn't match anything in the current cache, so
	// check and clean up old entries
	entry = c.OldEntries[cacheKey+dirPath]
	if entry == nil {
		return EntryNotFound
	}

	entry.decrementDirectoryInUseCount()
	if !entry.inUse() {
		// done with this old entry, so clean it up
		delete(c.OldEntries, cacheKey+dirPath)
	}
	return nil
}

func (c *FileCache) Add(cacheKey, sourcePath string, size int64, cachingInfo CachingInfoType) (*CachedFile, error) {
	lock.Lock()
	defer lock.Unlock()

	oldEntry := c.Entries[cacheKey]

	c.makeRoom(size, "")

	c.Seq++
	uniqueName := fmt.Sprintf("%s-%d-%d", cacheKey, time.Now().UnixNano(), c.Seq)
	cachePath := filepath.Join(c.CachedPath, uniqueName)

	err := os.Rename(sourcePath, cachePath)
	if err != nil {
		return nil, err
	}

	newEntry := newFileCacheEntry(cachePath, size, cachingInfo)
	c.Entries[cacheKey] = newEntry
	if oldEntry != nil {
		oldEntry.decrementUse()
		c.updateOldEntries(cacheKey, oldEntry)
	}
	return newEntry.readCloser()
}

func (c *FileCache) AddDirectory(cacheKey, sourcePath string, size int64, cachingInfo CachingInfoType) (string, error) {
	lock.Lock()
	defer lock.Unlock()

	oldEntry := c.Entries[cacheKey]

	c.makeRoom(size, "")

	c.Seq++
	uniqueName := fmt.Sprintf("%s-%d-%d", cacheKey, time.Now().UnixNano(), c.Seq)
	cachePath := filepath.Join(c.CachedPath, uniqueName)

	err := os.Rename(sourcePath, cachePath)
	if err != nil {
		return "", err
	}
	newEntry := newFileCacheEntry(cachePath, size, cachingInfo)
	c.Entries[cacheKey] = newEntry
	if oldEntry != nil {
		oldEntry.decrementUse()
		c.updateOldEntries(cacheKey, oldEntry)
	}
	return newEntry.expandedDirectory()
}

func (c *FileCache) Get(cacheKey string) (*CachedFile, CachingInfoType, error) {
	lock.Lock()
	defer lock.Unlock()

	entry := c.Entries[cacheKey]
	if entry == nil {
		return nil, CachingInfoType{}, EntryNotFound
	}

	if entry.fileDoesNotExist() {
		c.makeRoom(entry.Size, cacheKey)
	}

	entry.Access = time.Now()
	readCloser, err := entry.readCloser()
	if err != nil {
		return nil, CachingInfoType{}, err
	}

	return readCloser, entry.CachingInfo, nil
}

func (c *FileCache) GetDirectory(cacheKey string) (string, CachingInfoType, error) {
	lock.Lock()
	defer lock.Unlock()

	entry := c.Entries[cacheKey]
	if entry == nil {
		return "", CachingInfoType{}, EntryNotFound
	}

	// Was it expanded before
	if entry.dirDoesNotExist() {
		// Do we have enough room to double the size?
		c.makeRoom(entry.Size, cacheKey)
		entry.Size = entry.Size * 2
	}

	entry.Access = time.Now()
	dir, err := entry.expandedDirectory()
	if err != nil {
		return "", CachingInfoType{}, err
	}

	return dir, entry.CachingInfo, nil
}

func (c *FileCache) Remove(cacheKey string) {
	lock.Lock()
	c.remove(cacheKey)
	lock.Unlock()
}

func (c *FileCache) remove(cacheKey string) {
	entry := c.Entries[cacheKey]
	if entry != nil {
		entry.decrementUse()
		c.updateOldEntries(cacheKey, entry)
		delete(c.Entries, cacheKey)
	}
}

func (c *FileCache) updateOldEntries(cacheKey string, entry *FileCacheEntry) {
	if entry != nil {
		if !entry.inUse() && entry.ExpandedDirectoryPath != "" {
			// put it in the oldEntries Cache since somebody may still be using the directory
			c.OldEntries[cacheKey+entry.ExpandedDirectoryPath] = entry
		} else {
			// We need to remove it from oldEntries
			delete(c.OldEntries, cacheKey+entry.ExpandedDirectoryPath)
		}
	}
}

func (c *FileCache) makeRoom(size int64, excludedCacheKey string) {
	usedSpace := c.usedSpace()
	for c.maxSizeInBytes < usedSpace+size {
		var oldestEntry *FileCacheEntry
		oldestAccessTime, oldestCacheKey := time.Now(), ""
		for ck, f := range c.Entries {
			if f.Access.Before(oldestAccessTime) && ck != excludedCacheKey && !f.inUse() {
				oldestAccessTime = f.Access
				oldestEntry = f
				oldestCacheKey = ck
			}
		}

		if oldestEntry == nil {
			// could not find anything we could remove
			return
		}

		usedSpace -= oldestEntry.Size
		c.remove(oldestCacheKey)
	}

	return
}

func (c *FileCache) usedSpace() int64 {
	space := int64(0)
	for _, f := range c.Entries {
		space += f.Size
	}
	return space
}

func extractTarToDirectory(sourcePath, destinationDir string) error {
	_, err := os.Stat(destinationDir)
	if err != nil && err.(*os.PathError).Err != syscall.ENOENT {
		return err
	}

	file, err := os.Open(sourcePath)
	if err != nil {
		return err
	}

	defer file.Close()

	var fileReader io.ReadCloser = file

	// Make the target directory
	err = os.MkdirAll(destinationDir, 0777)
	if err != nil {
		return err
	}

	tarBallReader := tar.NewReader(fileReader)
	// Extracting tarred files
	for {
		header, err := tarBallReader.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		// get the individual filename and extract to the current directory
		filename := header.Name

		switch header.Typeflag {
		case tar.TypeDir:
			// handle directory
			fullpath := filepath.Join(destinationDir, filename)
			err = os.MkdirAll(fullpath, os.FileMode(header.Mode))

			if err != nil {
				return err
			}

		default:
			// handle normal file
			fullpath := filepath.Join(destinationDir, filename)

			err := os.MkdirAll(filepath.Dir(fullpath), 0777)
			if err != nil {
				return err
			}

			writer, err := os.Create(fullpath)

			if err != nil {
				return err
			}

			io.Copy(writer, tarBallReader)

			err = os.Chmod(fullpath, os.FileMode(header.Mode))

			if err != nil {
				return err
			}

			writer.Close()

		}
	}
	return nil
}
