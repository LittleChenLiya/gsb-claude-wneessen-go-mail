// SPDX-FileCopyrightText: The go-mail Authors
//
// SPDX-License-Identifier: MIT

package mail

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

// List of sentinel errors returned by the attachment snapshot API.
var (
	// ErrSnapshotNil is returned when a nil AttachmentSnapshot is attached to a Msg.
	ErrSnapshotNil = errors.New("attachment snapshot is nil")

	// ErrSnapshotConsumed is returned when a non-replayable (single-use) attachment snapshot
	// is used again after its source reader has already been consumed. A consumed reader is
	// never silently reused for a send retry; the caller must build a new snapshot from a
	// fresh source via the SnapshotFactory.
	ErrSnapshotConsumed = errors.New("attachment snapshot is already consumed and not replayable")

	// ErrSnapshotModified is returned when a file-backed attachment snapshot detects that the
	// source file on disk no longer matches the identity (size, modification time and
	// SHA-256 content hash) that was recorded when the snapshot was built.
	ErrSnapshotModified = errors.New("attachment source file has been modified since the snapshot was built")

	// ErrSnapshotCacheExhausted is returned when caching an attachment in memory would exceed
	// the total cache limit of the SnapshotFactory. The limit guards against reading a whole
	// batch of attachments into memory without bounds.
	ErrSnapshotCacheExhausted = errors.New("attachment snapshot cache limit exhausted")
)

// DefaultSnapshotCacheLimit is the default total in-memory cache budget (in bytes) of a
// SnapshotFactory. It caps the accumulated content of all reader-based snapshots built by a
// single factory, so a batch of attachments can never be read into memory without bounds.
const DefaultSnapshotCacheLimit int64 = 32 << 20 // 32 MiB

// AttachmentSnapshot is an explicit, immutable binding of an attachment name and its content,
// created in a single build step by a SnapshotFactory.
//
// A snapshot decouples attachment content from the send path:
//
//   - File-backed snapshots (SnapshotFactory.FromFile) record the file identity (size,
//     modification time and SHA-256 hash) at build time. On every send the file is
//     re-verified against that identity before any data is handed to the SMTP server. If the
//     file changed on disk, the send is rejected with ErrSnapshotModified and no (partial)
//     message is transmitted. To intentionally pick up a changed file, the caller must
//     explicitly rebuild the snapshot through the SnapshotFactory.
//   - Cached reader snapshots (SnapshotFactory.FromReader) copy the reader content into the
//     factory-bounded in-memory cache once. They are replayable: send retries re-use the
//     cached copy, never the original reader.
//   - Single-use reader snapshots (SnapshotFactory.FromReaderOnce) stream a non-replayable
//     io.Reader exactly once. A second use - e.g. a send retry - fails explicitly with
//     ErrSnapshotConsumed instead of silently re-reading an exhausted reader.
//
// The snapshot name is a display name only: any directory components are stripped when the
// snapshot is built, so a name is never interpreted as a file system path. Non-ASCII names
// are MIME encoded-word encoded by the Msg writer when the message is rendered.
type AttachmentSnapshot struct {
	// name is the display (base) name of the attachment. It is never a path.
	name string

	// size is the content length in bytes recorded at build time (-1 while unknown).
	size int64

	// sum is the SHA-256 hash of the content recorded at build (or consume) time.
	sum     [sha256.Size]byte
	hasSum  bool
	modTime time.Time

	// path is the source file path for file-backed snapshots (empty otherwise).
	path string

	// cached holds the in-memory copy of reader-based snapshots (nil for file-backed and
	// single-use snapshots).
	cached []byte

	// once marks a single-use, non-replayable snapshot streaming from reader.
	once     bool
	reader   io.Reader
	consumed bool

	// opts are the FileOption functions applied when the snapshot is attached to a Msg.
	opts []FileOption

	mu sync.Mutex
}

// Name returns the display name of the attachment snapshot. The name never contains path
// separators; directory components were stripped when the snapshot was built.
func (s *AttachmentSnapshot) Name() string {
	return s.name
}

// Size returns the content length in bytes recorded when the snapshot was built. For
// single-use snapshots that have not been consumed yet, Size returns -1.
func (s *AttachmentSnapshot) Size() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size
}

// Sum returns the SHA-256 hash of the snapshot content as recorded at build time (or, for
// single-use snapshots, when the content was consumed). The second return value reports
// whether the hash is known yet.
func (s *AttachmentSnapshot) Sum() ([sha256.Size]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sum, s.hasSum
}

// Replayable reports whether the snapshot can be written more than once. File-backed and
// cached reader snapshots are replayable; single-use reader snapshots are not.
func (s *AttachmentSnapshot) Replayable() bool {
	return !s.once
}

// Consumed reports whether a single-use snapshot has already streamed its source reader.
func (s *AttachmentSnapshot) Consumed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.consumed
}

// verify performs the pre-delivery checks of the snapshot. It is called by the Client before
// any SMTP command for the message is issued, guaranteeing that a failed check never hands a
// (partial) message to the SMTP server.
func (s *AttachmentSnapshot) verify() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.once && s.consumed {
		return fmt.Errorf("%w: attachment %q", ErrSnapshotConsumed, s.name)
	}
	if s.path != "" {
		return s.verifyFileLocked()
	}
	return nil
}

// verifyFileLocked re-checks the on-disk identity of a file-backed snapshot against the
// identity recorded at build time. The file is streamed through a hash, so memory usage stays
// bounded regardless of file size.
func (s *AttachmentSnapshot) verifyFileLocked() error {
	info, err := os.Stat(s.path)
	if err != nil {
		return fmt.Errorf("failed to stat attachment file %q: %w", s.name, err)
	}
	if info.Size() != s.size || !info.ModTime().Equal(s.modTime) {
		return fmt.Errorf("%w: attachment %q", ErrSnapshotModified, s.name)
	}
	file, err := os.Open(s.path)
	if err != nil {
		return fmt.Errorf("failed to open attachment file %q: %w", s.name, err)
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	numBytes, err := io.Copy(hash, file)
	if err != nil {
		return fmt.Errorf("failed to read attachment file %q: %w", s.name, err)
	}
	var sum [sha256.Size]byte
	copy(sum[:], hash.Sum(nil))
	if numBytes != s.size || sum != s.sum {
		return fmt.Errorf("%w: attachment %q", ErrSnapshotModified, s.name)
	}
	return nil
}

// writeTo streams the snapshot content to w, enforcing the snapshot semantics: cached
// snapshots replay from memory, file-backed snapshots re-verify and stream from disk, and
// single-use snapshots stream their reader exactly once.
func (s *AttachmentSnapshot) writeTo(w io.Writer) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.once:
		if s.consumed {
			return 0, fmt.Errorf("%w: attachment %q", ErrSnapshotConsumed, s.name)
		}
		s.consumed = true
		hash := sha256.New()
		numBytes, err := io.Copy(io.MultiWriter(w, hash), s.reader)
		s.size = numBytes
		copy(s.sum[:], hash.Sum(nil))
		s.hasSum = true
		if err != nil {
			return numBytes, fmt.Errorf("failed to stream attachment %q: %w", s.name, err)
		}
		return numBytes, nil
	case s.path != "":
		if err := s.verifyFileLocked(); err != nil {
			return 0, err
		}
		file, err := os.Open(s.path)
		if err != nil {
			return 0, fmt.Errorf("failed to open attachment file %q: %w", s.name, err)
		}
		defer func() { _ = file.Close() }()
		hash := sha256.New()
		numBytes, err := io.Copy(io.MultiWriter(w, hash), file)
		if err != nil {
			return numBytes, fmt.Errorf("failed to stream attachment %q: %w", s.name, err)
		}
		var sum [sha256.Size]byte
		copy(sum[:], hash.Sum(nil))
		if numBytes != s.size || sum != s.sum {
			return numBytes, fmt.Errorf("%w: attachment %q", ErrSnapshotModified, s.name)
		}
		return numBytes, nil
	default:
		numBytes, err := w.Write(s.cached)
		return int64(numBytes), err
	}
}

// SnapshotFactory builds AttachmentSnapshot values. Each snapshot binds an attachment name
// and its content identity in a single, explicit build step.
//
// The factory owns a total in-memory cache budget (cacheLimit bytes). Every snapshot built
// with FromReader caches its content in memory and charges it against this budget; once the
// budget would be exceeded, building fails with ErrSnapshotCacheExhausted. This guarantees
// that a whole batch of reader attachments is never read into memory without bounds.
// File-backed snapshots are streamed from disk and do not consume the cache budget.
//
// A SnapshotFactory is not safe for concurrent use; build snapshots from a single goroutine
// or provide external synchronization.
type SnapshotFactory struct {
	limit int64
	used  int64
}

// NewSnapshotFactory returns a SnapshotFactory with the given total in-memory cache limit in
// bytes. A cacheLimit <= 0 selects DefaultSnapshotCacheLimit.
func NewSnapshotFactory(cacheLimit int64) *SnapshotFactory {
	if cacheLimit <= 0 {
		cacheLimit = DefaultSnapshotCacheLimit
	}
	return &SnapshotFactory{limit: cacheLimit}
}

// CacheLimit returns the total in-memory cache budget of the SnapshotFactory in bytes.
func (f *SnapshotFactory) CacheLimit() int64 {
	return f.limit
}

// CacheUsed returns the number of cache bytes already consumed by snapshots built with
// SnapshotFactory.FromReader.
func (f *SnapshotFactory) CacheUsed() int64 {
	return f.used
}

// FromFile builds a file-backed AttachmentSnapshot for the file at the given path.
//
// The file content is streamed through a SHA-256 hash (constant memory usage) and the file
// identity (size, modification time, hash) is recorded in the snapshot. The snapshot does
// not hold the content in memory; on every send the file is re-verified against the recorded
// identity and streamed from disk. If the file has been modified since the snapshot was
// built, the send is rejected with ErrSnapshotModified and the snapshot must be explicitly
// rebuilt through the factory to pick up the new content.
//
// The snapshot name is the base name of the path; directory components are never used as the
// attachment name.
func (f *SnapshotFactory) FromFile(filePath string, opts ...FileOption) (*AttachmentSnapshot, error) {
	if filePath == "" {
		return nil, errors.New("attachment file path must not be empty")
	}
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open attachment file: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat attachment file %q: %w", filePath, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("attachment path %q is a directory", filePath)
	}
	hash := sha256.New()
	numBytes, err := io.Copy(hash, file)
	if err != nil {
		return nil, fmt.Errorf("failed to read attachment file %q: %w", filePath, err)
	}
	snapshot := &AttachmentSnapshot{
		name:    snapshotBaseName(filePath),
		size:    numBytes,
		modTime: info.ModTime(),
		path:    filePath,
		opts:    opts,
		hasSum:  true,
	}
	copy(snapshot.sum[:], hash.Sum(nil))
	return snapshot, nil
}

// FromReader builds a replayable AttachmentSnapshot by reading the content of the given
// io.Reader into the factory-bounded in-memory cache. The reader is consumed exactly once,
// at build time; sends and send retries afterwards replay the cached copy and never touch
// the reader again.
//
// If caching the content would exceed the total cache limit of the factory, FromReader fails
// with ErrSnapshotCacheExhausted and no snapshot is created.
//
// The given name is a display name only: directory components are stripped and the name is
// never interpreted as a file system path.
func (f *SnapshotFactory) FromReader(name string, reader io.Reader, opts ...FileOption) (*AttachmentSnapshot, error) {
	if reader == nil {
		return nil, errors.New("io.Reader must not be nil")
	}
	remaining := f.limit - f.used
	data, err := io.ReadAll(io.LimitReader(reader, remaining+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read attachment %q content: %w", name, err)
	}
	if int64(len(data)) > remaining {
		return nil, fmt.Errorf("%w: attachment %q exceeds the remaining snapshot cache budget "+
			"(limit: %d bytes, used: %d bytes)", ErrSnapshotCacheExhausted, name, f.limit, f.used)
	}
	f.used += int64(len(data))
	hash := sha256.New()
	_, _ = hash.Write(data)
	snapshot := &AttachmentSnapshot{
		name:   snapshotBaseName(name),
		size:   int64(len(data)),
		cached: data,
		opts:   opts,
		hasSum: true,
	}
	copy(snapshot.sum[:], hash.Sum(nil))
	return snapshot, nil
}

// FromReaderOnce builds a single-use, non-replayable AttachmentSnapshot that streams the
// given io.Reader directly when the message is rendered. The reader is not touched at build
// time and is consumed by the first send. Any further use - for example a send retry -
// fails explicitly with ErrSnapshotConsumed instead of silently re-reading an exhausted
// reader. To send again, build a new snapshot from a fresh reader.
//
// Single-use snapshots do not consume the cache budget of the factory, but they also cannot
// be verified before the SMTP DATA phase; prefer FromReader or FromFile when retries must be
// safe.
//
// The given name is a display name only: directory components are stripped and the name is
// never interpreted as a file system path.
func (f *SnapshotFactory) FromReaderOnce(name string, reader io.Reader, opts ...FileOption) (*AttachmentSnapshot, error) {
	if reader == nil {
		return nil, errors.New("io.Reader must not be nil")
	}
	return &AttachmentSnapshot{
		name:   snapshotBaseName(name),
		size:   -1,
		once:   true,
		reader: reader,
		opts:   opts,
	}, nil
}

// snapshotBaseName reduces a user-provided attachment name to its final path element. Both
// forward and backward slashes are treated as separators, independent of the host operating
// system, so a snapshot name is always a display name and never a file system path.
func snapshotBaseName(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	name = path.Base(name)
	if name == "" || name == "." || name == "/" {
		return "attachment"
	}
	return name
}

// AttachSnapshot attaches an AttachmentSnapshot to the Msg.
//
// The snapshot becomes a regular attachment of the message. Before the Client hands anything
// to the SMTP server, all snapshots of the message are verified: file-backed snapshots are
// re-checked against their recorded on-disk identity and single-use snapshots are checked
// for prior consumption. If a check fails, the send is rejected before the first SMTP
// command, so no incomplete message is ever transmitted.
//
// Parameters:
//   - snapshot: The AttachmentSnapshot to attach (must not be nil).
//
// Returns:
//   - An error if the snapshot is nil, otherwise nil.
func (m *Msg) AttachSnapshot(snapshot *AttachmentSnapshot) error {
	if snapshot == nil {
		return ErrSnapshotNil
	}
	file := &File{
		Name:   snapshot.name,
		Header: make(map[string][]string),
		Writer: snapshot.writeTo,
	}
	m.attachments = m.appendFile(m.attachments, file, snapshot.opts...)
	m.snapshots = append(m.snapshots, snapshot)
	return nil
}

// verifyAttachmentSnapshots runs the pre-delivery verification of all attachment snapshots
// of the Msg. It returns the first verification error encountered, or nil if all snapshots
// are safe to send.
func (m *Msg) verifyAttachmentSnapshots() error {
	for _, snapshot := range m.snapshots {
		if snapshot == nil {
			continue
		}
		if err := snapshot.verify(); err != nil {
			return err
		}
	}
	return nil
}
