// SPDX-FileCopyrightText: The go-mail Authors
//
// SPDX-License-Identifier: MIT

package mail

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	// ErrSnapshotFileChanged indicates that a file-backed attachment source changed (or
	// disappeared) after the AttachmentSnapshot was taken. The snapshot rejects the build
	// and a new snapshot has to be created explicitly via the Snapshotter factory.
	ErrSnapshotFileChanged = errors.New("attachment source file changed since snapshot was taken")

	// ErrSnapshotConsumed indicates that a non-replayable (one-shot) attachment reader has
	// already been consumed. A consumed reader must never be reused directly, e.g. by a
	// send retry. Bind a new reader instead.
	ErrSnapshotConsumed = errors.New("attachment snapshot reader already consumed and cannot be replayed")

	// ErrSnapshotCacheLimit indicates that caching an attachment would exceed the total
	// in-memory cache budget of the Snapshotter. The attachment is refused instead of
	// being read into memory without bounds.
	ErrSnapshotCacheLimit = errors.New("attachment snapshot cache limit exceeded")
)

// DefaultSnapshotCacheLimit is the default total amount of memory (in bytes) a Snapshotter
// will use to cache attachment content. Attachments that do not fit into the remaining
// budget are either streamed (file-backed snapshots) or refused (reader-backed snapshots).
const DefaultSnapshotCacheLimit int64 = 32 << 20 // 32 MiB

// SnapshotterOption is a function type used to modify properties of a Snapshotter.
type SnapshotterOption func(*Snapshotter)

// WithSnapshotCacheLimit sets the total in-memory cache budget (in bytes) of a Snapshotter.
//
// The cache budget is shared by all snapshots created by the Snapshotter. Content that
// fits into the remaining budget is buffered in memory so it can be replayed (e.g. by
// send retries) and is protected against concurrent modification. File-backed snapshots
// that exceed the budget are streamed from disk instead (still guarded by the snapshot
// identity), while reader-backed snapshots that exceed the budget are refused with
// ErrSnapshotCacheLimit. A negative limit disables caching entirely.
//
// Parameters:
//   - limit: The total number of bytes the Snapshotter may cache in memory.
//
// Returns:
//   - A SnapshotterOption function that sets the cache limit.
func WithSnapshotCacheLimit(limit int64) SnapshotterOption {
	return func(s *Snapshotter) {
		s.cacheLimit = limit
	}
}

// Snapshotter is an explicit factory for attachment snapshots.
//
// A Snapshotter binds an attachment's content source and its display name in a single
// build round, producing an AttachmentSnapshot. The snapshot carries an identity of the
// source (size, modification time and SHA-256 checksum for file-backed snapshots) so
// that later builds can verify the source is still the same content that was snapshotted.
//
// The Snapshotter keeps a bounded in-memory cache (see WithSnapshotCacheLimit). It never
// reads attachment content into memory beyond this budget: small content is cached for
// replayability, large file-backed content is streamed from disk under identity
// verification, and oversized reader-backed content is refused with ErrSnapshotCacheLimit.
//
// The zero value is not ready for use; create a Snapshotter with NewSnapshotter.
type Snapshotter struct {
	cacheLimit int64
	cacheUsed  int64
	mutex      sync.Mutex
}

// NewSnapshotter creates a new Snapshotter, optionally configured with the given
// SnapshotterOption functions.
//
// Parameters:
//   - opts: Optional SnapshotterOption functions to customize the Snapshotter.
//
// Returns:
//   - A pointer to the newly created Snapshotter.
func NewSnapshotter(opts ...SnapshotterOption) *Snapshotter {
	snapshotter := &Snapshotter{cacheLimit: DefaultSnapshotCacheLimit}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt(snapshotter)
	}
	return snapshotter
}

// SnapshotFile creates an AttachmentSnapshot for a file on the local file system.
//
// The snapshot binds the given path and a display name (the base name of the path, unless
// overridden with WithFileName) in the current build round and records the file's identity
// (size, modification time and SHA-256 checksum). The display name is only used for the
// MIME headers of the attachment and is never interpreted as a file system path.
//
// If the file fits into the remaining cache budget of the Snapshotter, its content is
// cached in memory; otherwise it is streamed from disk on each build, guarded by the
// recorded identity. If the file changes after the snapshot was taken, building the
// snapshot fails with ErrSnapshotFileChanged and a new snapshot has to be created
// explicitly (i.e. by calling SnapshotFile again).
//
// Parameters:
//   - path: The path of the file to snapshot.
//   - opts: Optional FileOption functions to customize the attachment.
//
// Returns:
//   - A pointer to the newly created AttachmentSnapshot.
//   - An error if the file cannot be accessed or read.
func (s *Snapshotter) SnapshotFile(path string, opts ...FileOption) (*AttachmentSnapshot, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("snapshot: failed to stat file %q: %w", path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("snapshot: %q is a directory, not a file", path)
	}

	snapshot := &AttachmentSnapshot{
		name:    filepath.Base(path),
		path:    path,
		modTime: info.ModTime(),
		size:    info.Size(),
		opts:    opts,
	}
	if s.reserveCache(info.Size()) {
		snapshot.data, err = os.ReadFile(path)
		if err != nil {
			s.releaseCache(info.Size())
			return nil, fmt.Errorf("snapshot: failed to read file %q: %w", path, err)
		}
		snapshot.cached = true
		snapshot.sum = sha256.Sum256(snapshot.data)
	} else {
		snapshot.sum, err = hashFileSum(path)
		if err != nil {
			return nil, fmt.Errorf("snapshot: failed to hash file %q: %w", path, err)
		}
	}
	snapshot.bindName()
	return snapshot, nil
}

// SnapshotReader creates an AttachmentSnapshot by fully reading the given io.Reader.
//
// The snapshot binds the reader's content and the given display name in the current build
// round. The content is stored in the Snapshotter's bounded in-memory cache, making the
// resulting snapshot replayable any number of times (e.g. by send retries). If the
// content exceeds the remaining cache budget, SnapshotReader refuses it with
// ErrSnapshotCacheLimit instead of reading unboundedly into memory; use SnapshotFile or
// BindReader for large or streaming sources.
//
// The name is only used for the MIME headers of the attachment and is never interpreted
// as a file system path.
//
// Parameters:
//   - name: The display name of the attachment.
//   - reader: The io.Reader providing the attachment content.
//   - opts: Optional FileOption functions to customize the attachment.
//
// Returns:
//   - A pointer to the newly created AttachmentSnapshot.
//   - An error if the reader fails or the content exceeds the cache budget.
func (s *Snapshotter) SnapshotReader(name string, reader io.Reader, opts ...FileOption) (*AttachmentSnapshot, error) {
	if reader == nil {
		return nil, errors.New("snapshot: reader must not be nil")
	}
	remaining := s.cacheRemaining()
	if remaining < 0 {
		remaining = 0
	}
	data, err := readBounded(reader, remaining)
	if err != nil {
		return nil, err
	}
	s.reserveCache(int64(len(data)))
	snapshot := &AttachmentSnapshot{
		name:   name,
		size:   int64(len(data)),
		sum:    sha256.Sum256(data),
		cached: true,
		data:   data,
		opts:   opts,
	}
	snapshot.bindName()
	return snapshot, nil
}

// BindReader creates a non-replayable, one-shot AttachmentSnapshot from an io.Reader.
//
// In contrast to SnapshotReader, the reader is not consumed at bind time and its content
// is not cached in memory. Instead, the reader is streamed exactly once, when the
// snapshot is built into a message. After that, the snapshot is consumed: any further
// build (e.g. caused by a send retry) fails explicitly with ErrSnapshotConsumed, so a
// consumed reader is never silently reused. To send again, bind a new reader.
//
// The name is only used for the MIME headers of the attachment and is never interpreted
// as a file system path.
//
// Parameters:
//   - name: The display name of the attachment.
//   - reader: The io.Reader providing the attachment content. It must not be nil.
//   - opts: Optional FileOption functions to customize the attachment.
//
// Returns:
//   - A pointer to the newly created AttachmentSnapshot.
//   - An error if the reader is nil.
func (s *Snapshotter) BindReader(name string, reader io.Reader, opts ...FileOption) (*AttachmentSnapshot, error) {
	if reader == nil {
		return nil, errors.New("snapshot: reader must not be nil")
	}
	snapshot := &AttachmentSnapshot{name: name, oneShot: reader, opts: opts}
	snapshot.bindName()
	return snapshot, nil
}

// reserveCache attempts to reserve the given amount of the Snapshotter's cache budget and
// reports whether the reservation was successful.
func (s *Snapshotter) reserveCache(size int64) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.cacheLimit < 0 || s.cacheUsed+size > s.cacheLimit {
		return false
	}
	s.cacheUsed += size
	return true
}

// releaseCache releases a previously reserved amount of the Snapshotter's cache budget.
func (s *Snapshotter) releaseCache(size int64) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.cacheUsed -= size
	if s.cacheUsed < 0 {
		s.cacheUsed = 0
	}
}

// cacheRemaining returns the remaining cache budget of the Snapshotter in bytes. A
// negative return value indicates that no caching is possible.
func (s *Snapshotter) cacheRemaining() int64 {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.cacheLimit < 0 {
		return -1
	}
	return s.cacheLimit - s.cacheUsed
}

// AttachmentSnapshot is an explicit, verified snapshot of a single attachment.
//
// An AttachmentSnapshot binds an attachment's content source and display name in one
// build round. It is created by a Snapshotter (the explicit factory) via SnapshotFile,
// SnapshotReader or BindReader. Each call to File verifies the snapshot before producing
// a File that can be attached to a Msg:
//
//   - File-backed snapshots verify that the source file still has the identity recorded
//     in the current round (size, modification time, SHA-256 checksum). If the file
//     changed, the build is rejected with ErrSnapshotFileChanged; create a new snapshot
//     via the Snapshotter to pick up the new content.
//   - Cached snapshots (small files and SnapshotReader content) replay their cached
//     content identically on every build.
//   - One-shot snapshots (BindReader) stream their reader exactly once. A second build
//     fails explicitly with ErrSnapshotConsumed.
//
// The display name of a snapshot is only used for the MIME headers of the attachment
// (where it is sanitized and encoded as an RFC 2047 encoded-word if required). It is
// never interpreted as a file system path.
type AttachmentSnapshot struct {
	// name is the display name of the attachment. It is never used as a file path.
	name string

	// opts are the FileOption functions applied to each File built from the snapshot.
	opts []FileOption

	// path is the source path of a file-backed snapshot. It is empty for reader-backed
	// snapshots.
	path string

	// modTime is the modification time of the source file recorded at snapshot time.
	modTime time.Time

	// size is the size of the source recorded at snapshot time.
	size int64

	// sum is the SHA-256 checksum of the content recorded at snapshot time.
	sum [sha256.Size]byte

	// cached indicates whether the content is held in the Snapshotter's in-memory cache.
	cached bool

	// data holds the cached content of the snapshot.
	data []byte

	// oneShot is a non-replayable reader bound by BindReader.
	oneShot io.Reader

	// consumed indicates whether a one-shot reader has already been streamed.
	consumed bool

	// mutex guards the consumed flag of one-shot snapshots.
	mutex sync.Mutex
}

// bindName binds the final display name of the snapshot in the current build round by
// applying the FileOption functions (e.g. WithFileName) to a probe File. The bound name
// is only a display name for the MIME headers and is never interpreted as a file system
// path.
func (s *AttachmentSnapshot) bindName() {
	probe := &File{Name: s.name, Header: make(map[string][]string)}
	for _, opt := range s.opts {
		if opt == nil {
			continue
		}
		opt(probe)
	}
	s.name = probe.Name
}

// Name returns the display name of the AttachmentSnapshot.
//
// Returns:
//   - The display name of the attachment.
func (s *AttachmentSnapshot) Name() string {
	return s.name
}

// Size returns the size of the snapshotted content in bytes. For one-shot snapshots
// created with BindReader the size is unknown and Size returns -1.
//
// Returns:
//   - The size of the snapshotted content in bytes, or -1 if unknown.
func (s *AttachmentSnapshot) Size() int64 {
	if s.oneShot != nil {
		return -1
	}
	return s.size
}

// File builds a File from the AttachmentSnapshot that can be attached to a Msg.
//
// The build verifies the snapshot as described on AttachmentSnapshot: file-backed
// snapshots reject changed sources with ErrSnapshotFileChanged and one-shot snapshots
// reject a second use with ErrSnapshotConsumed. Any FileOption functions passed to the
// Snapshotter factory method are applied to the returned File.
//
// Returns:
//   - A pointer to the File representing the snapshot in this build round.
//   - An error if the snapshot verification fails.
func (s *AttachmentSnapshot) File() (*File, error) {
	file := &File{Name: s.name, Header: make(map[string][]string)}
	switch {
	case s.path != "":
		if err := s.verifyFile(); err != nil {
			return nil, err
		}
		if s.cached {
			file.Writer = s.cachedFileWriter
		} else {
			file.Writer = s.verifiedFileWriter
		}
	case s.oneShot != nil:
		s.mutex.Lock()
		consumed := s.consumed
		s.mutex.Unlock()
		if consumed {
			return nil, fmt.Errorf("%w: one-shot attachment %q", ErrSnapshotConsumed, s.name)
		}
		file.Writer = s.oneShotWriter
	default:
		file.Writer = s.cachedWriter
	}
	for _, opt := range s.opts {
		if opt == nil {
			continue
		}
		opt(file)
	}
	return file, nil
}

// AttachTo builds a File from the AttachmentSnapshot and adds it as an attachment to the
// given Msg. It is a convenience method equivalent to calling File and appending the
// result to the Msg's attachments.
//
// Parameters:
//   - msg: The Msg to attach the snapshot to.
//
// Returns:
//   - An error if the snapshot verification fails.
func (s *AttachmentSnapshot) AttachTo(msg *Msg) error {
	file, err := s.File()
	if err != nil {
		return err
	}
	msg.attachments = append(msg.attachments, file)
	return nil
}

// verifyFile verifies that the file-backed source of the snapshot still has the identity
// recorded when the snapshot was taken.
//
// Returns:
//   - nil if the file is unchanged, ErrSnapshotFileChanged if it changed, or another
//     error if the file cannot be verified.
func (s *AttachmentSnapshot) verifyFile() error {
	info, err := os.Stat(s.path)
	if err != nil {
		return fmt.Errorf("snapshot: failed to verify file %q: %w", s.path, err)
	}
	if info.IsDir() || info.Size() != s.size || !info.ModTime().Equal(s.modTime) {
		return fmt.Errorf("%w: %s", ErrSnapshotFileChanged, s.path)
	}
	sum, err := hashFileSum(s.path)
	if err != nil {
		return fmt.Errorf("snapshot: failed to verify file %q: %w", s.path, err)
	}
	if sum != s.sum {
		return fmt.Errorf("%w: %s", ErrSnapshotFileChanged, s.path)
	}
	return nil
}

// cachedWriter returns a Writer function that replays the cached content of the snapshot.
// It is replayable any number of times.
func (s *AttachmentSnapshot) cachedWriter(writer io.Writer) (int64, error) {
	numBytes, err := io.Copy(writer, bytes.NewReader(s.data))
	if err != nil {
		return numBytes, fmt.Errorf("snapshot: failed to write cached attachment %q: %w", s.name, err)
	}
	return numBytes, nil
}

// cachedFileWriter replays the cached content of a file-backed snapshot, but only after
// verifying that the source file still carries the identity recorded at snapshot time.
// If the file changed, the write is rejected with ErrSnapshotFileChanged, even though the
// content is cached.
func (s *AttachmentSnapshot) cachedFileWriter(writer io.Writer) (int64, error) {
	if err := s.verifyFile(); err != nil {
		return 0, err
	}
	return s.cachedWriter(writer)
}

// verifiedFileWriter streams the content of a file-backed snapshot from disk while
// recomputing its checksum. If the content does not match the identity recorded at
// snapshot time, the write fails with ErrSnapshotFileChanged so that no silently altered
// content is delivered.
func (s *AttachmentSnapshot) verifiedFileWriter(writer io.Writer) (int64, error) {
	file, err := os.Open(s.path)
	if err != nil {
		return 0, fmt.Errorf("snapshot: failed to open file %q: %w", s.path, err)
	}
	hash := sha256.New()
	numBytes, copyErr := io.Copy(io.MultiWriter(writer, hash), file)
	if closeErr := file.Close(); closeErr != nil && copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return numBytes, fmt.Errorf("snapshot: failed to stream file %q: %w", s.path, copyErr)
	}
	var sum [sha256.Size]byte
	copy(sum[:], hash.Sum(nil))
	if numBytes != s.size || sum != s.sum {
		return numBytes, fmt.Errorf("%w: %s", ErrSnapshotFileChanged, s.path)
	}
	return numBytes, nil
}

// oneShotWriter streams the bound one-shot reader exactly once. Any subsequent call fails
// with ErrSnapshotConsumed so that a consumed reader is never reused directly.
func (s *AttachmentSnapshot) oneShotWriter(writer io.Writer) (int64, error) {
	s.mutex.Lock()
	if s.consumed {
		s.mutex.Unlock()
		return 0, fmt.Errorf("%w: one-shot attachment %q", ErrSnapshotConsumed, s.name)
	}
	s.consumed = true
	s.mutex.Unlock()
	numBytes, err := io.Copy(writer, s.oneShot)
	if err != nil {
		return numBytes, fmt.Errorf("snapshot: failed to read one-shot attachment %q: %w", s.name, err)
	}
	return numBytes, nil
}

// hashFileSum streams the file at the given path and returns its SHA-256 checksum without
// loading the whole file into memory.
func hashFileSum(path string) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	file, err := os.Open(path)
	if err != nil {
		return sum, err
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		_ = file.Close()
		return sum, err
	}
	if err = file.Close(); err != nil {
		return sum, err
	}
	copy(sum[:], hash.Sum(nil))
	return sum, nil
}

// readBounded reads at most limit bytes from the given reader. If the reader provides
// more than limit bytes, readBounded fails with ErrSnapshotCacheLimit instead of reading
// the content into memory without bounds.
func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	buffer := bytes.Buffer{}
	numBytes, err := io.Copy(&buffer, io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("snapshot: failed to read attachment content: %w", err)
	}
	if numBytes > limit {
		return nil, fmt.Errorf("%w: content exceeds remaining budget of %d bytes",
			ErrSnapshotCacheLimit, limit)
	}
	return buffer.Bytes(), nil
}
