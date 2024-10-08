// Package boltdb implements a BlobInfoCache backed by BoltDB.
package boltdb

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"time"

	"github.com/containers/image/v5/internal/blobinfocache"
	"github.com/containers/image/v5/pkg/blobinfocache/internal/prioritize"
	"github.com/containers/image/v5/types"
	"github.com/containers/storage/pkg/fileutils"
	"github.com/opencontainers/go-digest"
	"github.com/sirupsen/logrus"
	bolt "go.etcd.io/bbolt"
)

var (
	// NOTE: There is no versioning data inside the file; this is a “cache”, so on an incompatible format upgrade
	// we can simply start over with a different filename; update blobInfoCacheFilename.

	// FIXME: For CRI-O, does this need to hide information between different users?

	// uncompressedDigestBucket stores a mapping from any digest to an uncompressed digest.
	uncompressedDigestBucket = []byte("uncompressedDigest")
	// uncompressedDigestByTOCBucket stores a mapping from a TOC digest to an uncompressed digest.
	uncompressedDigestByTOCBucket = []byte("uncompressedDigestByTOC")
	// digestCompressorBucket stores a mapping from any digest to a compressor, or blobinfocache.Uncompressed (not blobinfocache.UnknownCompression).
	// It may not exist in caches created by older versions, even if uncompressedDigestBucket is present.
	digestCompressorBucket = []byte("digestCompressor")
	// digestSpecificVariantCompressorBucket stores a mapping from any digest to a (compressor, NUL byte, annotations as JSON), valid
	// only if digestCompressorBucket contains a value. The compressor is not `UnknownCompression`.
	digestSpecificVariantCompressorBucket = []byte("digestSpecificVariantCompressor")
	// It may not exist in caches created by older versions, even if digestCompressorBucket is present.
	// digestByUncompressedBucket stores a bucket per uncompressed digest, with the bucket containing a set of digests for that uncompressed digest
	// (as a set of key=digest, value="" pairs)
	digestByUncompressedBucket = []byte("digestByUncompressed")
	// knownLocationsBucket stores a nested structure of buckets, keyed by (transport name, scope string, blob digest), ultimately containing
	// a bucket of (opaque location reference, BinaryMarshaller-encoded time.Time value).
	knownLocationsBucket = []byte("knownLocations")
)

// Concurrency:
// See https://www.sqlite.org/src/artifact/c230a7a24?ln=994-1081 for all the issues with locks, which make it extremely
// difficult to use a single BoltDB file from multiple threads/goroutines inside a process.  So, we punt and only allow one at a time.

// pathLock contains a lock for a specific BoltDB database path.
type pathLock struct {
	refCount int64      // Number of threads/goroutines owning or waiting on this lock.  Protected by global pathLocksMutex, NOT by the mutex field below!
	mutex    sync.Mutex // Owned by the thread/goroutine allowed to access the BoltDB database.
}

var (
	// pathLocks contains a lock for each currently open file.
	// This must be global so that independently created instances of boltDBCache exclude each other.
	// The map is protected by pathLocksMutex.
	// FIXME? Should this be based on device:inode numbers instead of paths instead?
	pathLocks      = map[string]*pathLock{}
	pathLocksMutex = sync.Mutex{}
)

// lockPath obtains the pathLock for path.
// The caller must call unlockPath eventually.
func lockPath(path string) {
	pl := func() *pathLock { // A scope for defer
		pathLocksMutex.Lock()
		defer pathLocksMutex.Unlock()
		pl, ok := pathLocks[path]
		if ok {
			pl.refCount++
		} else {
			pl = &pathLock{refCount: 1, mutex: sync.Mutex{}}
			pathLocks[path] = pl
		}
		return pl
	}()
	pl.mutex.Lock()
}

// unlockPath releases the pathLock for path.
func unlockPath(path string) {
	pathLocksMutex.Lock()
	defer pathLocksMutex.Unlock()
	pl, ok := pathLocks[path]
	if !ok {
		// Should this return an error instead? BlobInfoCache ultimately ignores errors…
		panic(fmt.Sprintf("Internal error: unlocking nonexistent lock for path %s", path))
	}
	pl.mutex.Unlock()
	pl.refCount--
	if pl.refCount == 0 {
		delete(pathLocks, path)
	}
}

// cache is a BlobInfoCache implementation which uses a BoltDB file at the specified path.
//
// Note that we don’t keep the database open across operations, because that would lock the file and block any other
// users; instead, we need to open/close it for every single write or lookup.
type cache struct {
	path string
}

// New returns a BlobInfoCache implementation which uses a BoltDB file at path.
//
// Most users should call blobinfocache.DefaultCache instead.
//
// Deprecated: The BoltDB implementation triggers a panic() on some database format errors; that does not allow
// practical error recovery / fallback.
//
// Use blobinfocache.DefaultCache if at all possible; if not, the pkg/blobinfocache/sqlite implementation.
func New(path string) types.BlobInfoCache {
	return new2(path)
}
func new2(path string) *cache {
	return &cache{path: path}
}

// Open() sets up the cache for future accesses, potentially acquiring costly state. Each Open() must be paired with a Close().
// Note that public callers may call the types.BlobInfoCache operations without Open()/Close().
func (bdc *cache) Open() {
}

// Close destroys state created by Open().
func (bdc *cache) Close() {
}

// view returns runs the specified fn within a read-only transaction on the database.
func (bdc *cache) view(fn func(tx *bolt.Tx) error) (retErr error) {
	// bolt.Open(bdc.path, 0600, &bolt.Options{ReadOnly: true}) will, if the file does not exist,
	// nevertheless create it, but with an O_RDONLY file descriptor, try to initialize it, and fail — while holding
	// a read lock, blocking any future writes.
	// Hence this preliminary check, which is RACY: Another process could remove the file
	// between the Lexists call and opening the database.
	if err := fileutils.Lexists(bdc.path); err != nil && errors.Is(err, fs.ErrNotExist) {
		return err
	}

	lockPath(bdc.path)
	defer unlockPath(bdc.path)
	db, err := bolt.Open(bdc.path, 0600, &bolt.Options{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); retErr == nil && err != nil {
			retErr = err
		}
	}()

	return db.View(fn)
}

// update returns runs the specified fn within a read-write transaction on the database.
func (bdc *cache) update(fn func(tx *bolt.Tx) error) (retErr error) {
	lockPath(bdc.path)
	defer unlockPath(bdc.path)
	db, err := bolt.Open(bdc.path, 0600, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); retErr == nil && err != nil {
			retErr = err
		}
	}()

	return db.Update(fn)
}

// uncompressedDigest implements BlobInfoCache.UncompressedDigest within the provided read-only transaction.
func (bdc *cache) uncompressedDigest(tx *bolt.Tx, anyDigest digest.Digest) digest.Digest {
	if b := tx.Bucket(uncompressedDigestBucket); b != nil {
		if uncompressedBytes := b.Get([]byte(anyDigest.String())); uncompressedBytes != nil {
			d, err := digest.Parse(string(uncompressedBytes))
			if err == nil {
				return d
			}
			// FIXME? Log err (but throttle the log volume on repeated accesses)?
		}
	}
	// Presence in digestsByUncompressedBucket implies that anyDigest must already refer to an uncompressed digest.
	// This way we don't have to waste storage space with trivial (uncompressed, uncompressed) mappings
	// when we already record a (compressed, uncompressed) pair.
	if b := tx.Bucket(digestByUncompressedBucket); b != nil {
		if b = b.Bucket([]byte(anyDigest.String())); b != nil {
			c := b.Cursor()
			if k, _ := c.First(); k != nil { // The bucket is non-empty
				return anyDigest
			}
		}
	}
	return ""
}

// UncompressedDigest returns an uncompressed digest corresponding to anyDigest.
// May return anyDigest if it is known to be uncompressed.
// Returns "" if nothing is known about the digest (it may be compressed or uncompressed).
func (bdc *cache) UncompressedDigest(anyDigest digest.Digest) digest.Digest {
	var res digest.Digest
	if err := bdc.view(func(tx *bolt.Tx) error {
		res = bdc.uncompressedDigest(tx, anyDigest)
		return nil
	}); err != nil { // Including os.IsNotExist(err)
		return "" // FIXME? Log err (but throttle the log volume on repeated accesses)?
	}
	return res
}

// RecordDigestUncompressedPair records that the uncompressed version of anyDigest is uncompressed.
// It’s allowed for anyDigest == uncompressed.
// WARNING: Only call this for LOCALLY VERIFIED data; don’t record a digest pair just because some remote author claims so (e.g.
// because a manifest/config pair exists); otherwise the cache could be poisoned and allow substituting unexpected blobs.
// (Eventually, the DiffIDs in image config could detect the substitution, but that may be too late, and not all image formats contain that data.)
func (bdc *cache) RecordDigestUncompressedPair(anyDigest digest.Digest, uncompressed digest.Digest) {
	_ = bdc.update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(uncompressedDigestBucket)
		if err != nil {
			return err
		}
		key := []byte(anyDigest.String())
		if previousBytes := b.Get(key); previousBytes != nil {
			previous, err := digest.Parse(string(previousBytes))
			if err != nil {
				return err
			}
			if previous != uncompressed {
				logrus.Warnf("Uncompressed digest for blob %s previously recorded as %s, now %s", anyDigest, previous, uncompressed)
			}
		}
		if err := b.Put(key, []byte(uncompressed.String())); err != nil {
			return err
		}

		b, err = tx.CreateBucketIfNotExists(digestByUncompressedBucket)
		if err != nil {
			return err
		}
		b, err = b.CreateBucketIfNotExists([]byte(uncompressed.String()))
		if err != nil {
			return err
		}
		if err := b.Put([]byte(anyDigest.String()), []byte{}); err != nil { // Possibly writing the same []byte{} presence marker again.
			return err
		}
		return nil
	}) // FIXME? Log error (but throttle the log volume on repeated accesses)?
}

// UncompressedDigestForTOC returns an uncompressed digest corresponding to anyDigest.
// Returns "" if the uncompressed digest is unknown.
func (bdc *cache) UncompressedDigestForTOC(tocDigest digest.Digest) digest.Digest {
	var res digest.Digest
	if err := bdc.view(func(tx *bolt.Tx) error {
		if b := tx.Bucket(uncompressedDigestByTOCBucket); b != nil {
			if uncompressedBytes := b.Get([]byte(tocDigest.String())); uncompressedBytes != nil {
				d, err := digest.Parse(string(uncompressedBytes))
				if err == nil {
					res = d
					return nil
				}
				// FIXME? Log err (but throttle the log volume on repeated accesses)?
			}
		}
		res = ""
		return nil
	}); err != nil { // Including os.IsNotExist(err)
		return "" // FIXME? Log err (but throttle the log volume on repeated accesses)?
	}
	return res
}

// RecordTOCUncompressedPair records that the tocDigest corresponds to uncompressed.
// WARNING: Only call this for LOCALLY VERIFIED data; don’t record a digest pair just because some remote author claims so (e.g.
// because a manifest/config pair exists); otherwise the cache could be poisoned and allow substituting unexpected blobs.
// (Eventually, the DiffIDs in image config could detect the substitution, but that may be too late, and not all image formats contain that data.)
func (bdc *cache) RecordTOCUncompressedPair(tocDigest digest.Digest, uncompressed digest.Digest) {
	_ = bdc.update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(uncompressedDigestByTOCBucket)
		if err != nil {
			return err
		}
		key := []byte(tocDigest.String())
		if previousBytes := b.Get(key); previousBytes != nil {
			previous, err := digest.Parse(string(previousBytes))
			if err != nil {
				return err
			}
			if previous != uncompressed {
				logrus.Warnf("Uncompressed digest for blob with TOC %q previously recorded as %q, now %q", tocDigest, previous, uncompressed)
			}
		}
		if err := b.Put(key, []byte(uncompressed.String())); err != nil {
			return err
		}
		return nil
	}) // FIXME? Log error (but throttle the log volume on repeated accesses)?
}

// RecordDigestCompressorData records data for the blob with the specified digest.
// WARNING: Only call this with LOCALLY VERIFIED data:
//   - don’t record a compressor for a digest just because some remote author claims so
//     (e.g. because a manifest says so);
//   - don’t record the non-base variant or annotations if we are not _sure_ that the base variant
//     and the blob’s digest match the non-base variant’s annotations (e.g. because we saw them
//     in a manifest)
//
// otherwise the cache could be poisoned and cause us to make incorrect edits to type
// information in a manifest.
func (bdc *cache) RecordDigestCompressorData(anyDigest digest.Digest, data blobinfocache.DigestCompressorData) {
	_ = bdc.update(func(tx *bolt.Tx) error {
		key := []byte(anyDigest.String())

		b, err := tx.CreateBucketIfNotExists(digestCompressorBucket)
		if err != nil {
			return err
		}
		warned := false
		if previousBytes := b.Get(key); previousBytes != nil {
			if string(previousBytes) != data.BaseVariantCompressor {
				logrus.Warnf("Compressor for blob with digest %s previously recorded as %s, now %s", anyDigest, string(previousBytes), data.BaseVariantCompressor)
				warned = true
			}
		}
		if data.BaseVariantCompressor == blobinfocache.UnknownCompression {
			if err := b.Delete(key); err != nil {
				return err
			}
			if b := tx.Bucket(digestSpecificVariantCompressorBucket); b != nil {
				if err := b.Delete(key); err != nil {
					return err
				}
			}
		}
		if err := b.Put(key, []byte(data.BaseVariantCompressor)); err != nil {
			return err
		}

		if data.SpecificVariantCompressor != blobinfocache.UnknownCompression {
			b, err := tx.CreateBucketIfNotExists(digestSpecificVariantCompressorBucket)
			if err != nil {
				return err
			}
			if !warned { // Don’t warn twice about the same digest
				if previousBytes := b.Get(key); previousBytes != nil {
					if prevSVCBytes, _, ok := bytes.Cut(previousBytes, []byte{0}); ok {
						prevSVC := string(prevSVCBytes)
						if data.SpecificVariantCompressor != prevSVC {
							logrus.Warnf("Specific compressor for blob with digest %s previously recorded as %s, now %s", anyDigest, prevSVC, data.SpecificVariantCompressor)
						}
					}
				}
			}
			annotations, err := json.Marshal(data.SpecificVariantAnnotations)
			if err != nil {
				return err
			}
			data := bytes.Clone([]byte(data.SpecificVariantCompressor))
			data = append(data, 0)
			data = append(data, annotations...)
			if err := b.Put(key, data); err != nil {
				return err
			}
		}
		return nil
	}) // FIXME? Log error (but throttle the log volume on repeated accesses)?
}

// RecordKnownLocation records that a blob with the specified digest exists within the specified (transport, scope) scope,
// and can be reused given the opaque location data.
func (bdc *cache) RecordKnownLocation(transport types.ImageTransport, scope types.BICTransportScope, blobDigest digest.Digest, location types.BICLocationReference) {
	_ = bdc.update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(knownLocationsBucket)
		if err != nil {
			return err
		}
		b, err = b.CreateBucketIfNotExists([]byte(transport.Name()))
		if err != nil {
			return err
		}
		b, err = b.CreateBucketIfNotExists([]byte(scope.Opaque))
		if err != nil {
			return err
		}
		b, err = b.CreateBucketIfNotExists([]byte(blobDigest.String()))
		if err != nil {
			return err
		}
		value, err := time.Now().MarshalBinary()
		if err != nil {
			return err
		}
		if err := b.Put([]byte(location.Opaque), value); err != nil { // Possibly overwriting an older entry.
			return err
		}
		return nil
	}) // FIXME? Log error (but throttle the log volume on repeated accesses)?
}

// appendReplacementCandidates creates prioritize.CandidateWithTime values for digest in scopeBucket
// (which might be nil) with corresponding compression
// info from compressionBucket and specificVariantCompresssionBucket (which might be nil), and returns the result of appending them
// to candidates.
// v2Options is not nil if the caller is CandidateLocations2: this allows including candidates with unknown location, and filters out candidates
// with unknown compression.
func (bdc *cache) appendReplacementCandidates(candidates []prioritize.CandidateWithTime, scopeBucket, compressionBucket, specificVariantCompresssionBucket *bolt.Bucket,
	digest digest.Digest, v2Options *blobinfocache.CandidateLocations2Options) []prioritize.CandidateWithTime {
	digestKey := []byte(digest.String())
	compressionData := blobinfocache.DigestCompressorData{
		BaseVariantCompressor:      blobinfocache.UnknownCompression,
		SpecificVariantCompressor:  blobinfocache.UnknownCompression,
		SpecificVariantAnnotations: nil,
	}
	if compressionBucket != nil {
		// the bucket won't exist if the cache was created by a v1 implementation and
		// hasn't yet been updated by a v2 implementation
		if compressorNameValue := compressionBucket.Get(digestKey); len(compressorNameValue) > 0 {
			compressionData.BaseVariantCompressor = string(compressorNameValue)
		}
		if specificVariantCompresssionBucket != nil {
			if svcData := specificVariantCompresssionBucket.Get(digestKey); svcData != nil {
				if compressorBytes, annotationBytes, ok := bytes.Cut(svcData, []byte{0}); ok {
					compressionData.SpecificVariantCompressor = string(compressorBytes)
					if err := json.Unmarshal(annotationBytes, &compressionData.SpecificVariantAnnotations); err != nil {
						return candidates // FIXME? Log error (but throttle the log volume on repeated accesses)?
					}
				}
			}
		}
	}
	template := prioritize.CandidateTemplateWithCompression(v2Options, digest, compressionData)
	if template == nil {
		return candidates
	}

	var b *bolt.Bucket
	if scopeBucket != nil {
		b = scopeBucket.Bucket(digestKey)
	}
	if b != nil {
		_ = b.ForEach(func(k, v []byte) error {
			t := time.Time{}
			if err := t.UnmarshalBinary(v); err != nil {
				return err
			}
			candidates = append(candidates, template.CandidateWithLocation(types.BICLocationReference{Opaque: string(k)}, t))
			return nil
		}) // FIXME? Log error (but throttle the log volume on repeated accesses)?
	} else if v2Options != nil {
		candidates = append(candidates, template.CandidateWithUnknownLocation())
	}
	return candidates
}

// CandidateLocations2 returns a prioritized, limited, number of blobs and their locations (if known)
// that could possibly be reused within the specified (transport scope) (if they still
// exist, which is not guaranteed).
func (bdc *cache) CandidateLocations2(transport types.ImageTransport, scope types.BICTransportScope, primaryDigest digest.Digest, options blobinfocache.CandidateLocations2Options) []blobinfocache.BICReplacementCandidate2 {
	return bdc.candidateLocations(transport, scope, primaryDigest, options.CanSubstitute, &options)
}

// candidateLocations implements CandidateLocations / CandidateLocations2.
// v2Options is not nil if the caller is CandidateLocations2.
func (bdc *cache) candidateLocations(transport types.ImageTransport, scope types.BICTransportScope, primaryDigest digest.Digest, canSubstitute bool,
	v2Options *blobinfocache.CandidateLocations2Options) []blobinfocache.BICReplacementCandidate2 {
	res := []prioritize.CandidateWithTime{}
	var uncompressedDigestValue digest.Digest // = ""
	if err := bdc.view(func(tx *bolt.Tx) error {
		scopeBucket := tx.Bucket(knownLocationsBucket)
		if scopeBucket != nil {
			scopeBucket = scopeBucket.Bucket([]byte(transport.Name()))
		}
		if scopeBucket != nil {
			scopeBucket = scopeBucket.Bucket([]byte(scope.Opaque))
		}
		// compressionBucket and svCompressionBucket won't have been created if previous writers never recorded info about compression,
		// and we don't want to fail just because of that
		compressionBucket := tx.Bucket(digestCompressorBucket)
		specificVariantCompressionBucket := tx.Bucket(digestSpecificVariantCompressorBucket)

		res = bdc.appendReplacementCandidates(res, scopeBucket, compressionBucket, specificVariantCompressionBucket, primaryDigest, v2Options)
		if canSubstitute {
			if uncompressedDigestValue = bdc.uncompressedDigest(tx, primaryDigest); uncompressedDigestValue != "" {
				b := tx.Bucket(digestByUncompressedBucket)
				if b != nil {
					b = b.Bucket([]byte(uncompressedDigestValue.String()))
					if b != nil {
						if err := b.ForEach(func(k, _ []byte) error {
							d, err := digest.Parse(string(k))
							if err != nil {
								return err
							}
							if d != primaryDigest && d != uncompressedDigestValue {
								res = bdc.appendReplacementCandidates(res, scopeBucket, compressionBucket, specificVariantCompressionBucket, d, v2Options)
							}
							return nil
						}); err != nil {
							return err
						}
					}
				}
				if uncompressedDigestValue != primaryDigest {
					res = bdc.appendReplacementCandidates(res, scopeBucket, compressionBucket, specificVariantCompressionBucket, uncompressedDigestValue, v2Options)
				}
			}
		}
		return nil
	}); err != nil { // Including os.IsNotExist(err)
		return []blobinfocache.BICReplacementCandidate2{} // FIXME? Log err (but throttle the log volume on repeated accesses)?
	}

	return prioritize.DestructivelyPrioritizeReplacementCandidates(res, primaryDigest, uncompressedDigestValue)
}

// CandidateLocations returns a prioritized, limited, number of blobs and their locations that could possibly be reused
// within the specified (transport scope) (if they still exist, which is not guaranteed).
//
// If !canSubstitute, the returned candidates will match the submitted digest exactly; if canSubstitute,
// data from previous RecordDigestUncompressedPair calls is used to also look up variants of the blob which have the same
// uncompressed digest.
func (bdc *cache) CandidateLocations(transport types.ImageTransport, scope types.BICTransportScope, primaryDigest digest.Digest, canSubstitute bool) []types.BICReplacementCandidate {
	return blobinfocache.CandidateLocationsFromV2(bdc.candidateLocations(transport, scope, primaryDigest, canSubstitute, nil))
}