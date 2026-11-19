package worker

import (
	"context"
	"time"

	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/store"
)

const gcBatch = 500

// RunGC deletes chunk files whose reference count has been zero for longer
// than the grace period, then removes their rows.
func (r *Runner) RunGC(ctx context.Context) error {
	r.applySettings(ctx)
	cutoff := r.d.Now().Add(-r.o.GCGrace).UnixMilli()
	for {
		chunks, err := r.d.DB.UnreferencedChunks(ctx, cutoff, gcBatch)
		if err != nil {
			return err
		}
		if len(chunks) == 0 {
			break
		}
		var released []string
		var bytes int64
		for _, c := range chunks {
			if err := ctx.Err(); err != nil {
				return err
			}
			// A dedup hit from an in-flight upload refreshes the file's mtime;
			// leave such chunks alone until the upload has committed.
			if st, err := r.d.Chunks.Stat(ctx, c.Hash); err == nil && st.ModTime.After(r.d.Now().Add(-r.o.GCGrace)) {
				continue
			}
			if err := r.d.Chunks.Delete(ctx, c.Hash); err != nil {
				r.d.Log.Warn("gc delete failed", "hash", c.Hash, "err", err)
				continue
			}
			released = append(released, c.Hash)
			bytes += c.Size
		}
		if len(released) > 0 {
			if err := r.propose(ctx, &meta.ReleaseChunks{Hashes: released}); err != nil {
				return err
			}
		}
		r.update(func(s *Stats) {
			s.ChunksReleased += int64(len(released))
			s.BytesReclaimed += bytes
		})
		if len(chunks) < gcBatch || len(released) == 0 {
			break
		}
	}
	if r.d.Local != nil {
		_ = r.d.Local.CleanTemp(r.o.GCGrace)
	}
	r.update(func(s *Stats) { s.LastGC = r.d.Now() })
	return nil
}

// RunOrphanScan walks the local chunk directory and removes files that
// metadata has never heard of and that are older than the grace period:
// leftovers of uploads that failed before their metadata committed.
func (r *Runner) RunOrphanScan(ctx context.Context) error {
	if r.d.Local == nil {
		return nil
	}
	cutoff := r.d.Now().Add(-r.o.GCGrace)
	var removed, bytes int64
	err := r.d.Local.Walk(ctx, func(ci store.ChunkInfo) error {
		if ci.ModTime.After(cutoff) {
			return nil
		}
		known, err := r.d.DB.ChunkKnown(ctx, ci.Hash)
		if err != nil {
			return err
		}
		if known {
			return nil
		}
		if err := r.d.Local.Delete(ctx, ci.Hash); err != nil {
			r.d.Log.Warn("orphan delete failed", "hash", ci.Hash, "err", err)
			return nil
		}
		removed++
		bytes += ci.Size
		return nil
	})
	r.update(func(s *Stats) {
		s.OrphansRemoved += removed
		s.BytesReclaimed += bytes
	})
	return err
}

// RunUploadCleanup aborts multipart uploads older than UploadMaxAge and
// forgets completed-upload records older than a day.
func (r *Runner) RunUploadCleanup(ctx context.Context) error {
	r.applySettings(ctx)
	if err := r.propose(ctx, &meta.PurgeCompletedUploads{Before: r.d.Now().Add(-24 * time.Hour).UnixMilli()}); err != nil {
		return err
	}
	if r.o.UploadMaxAge <= 0 {
		return nil
	}
	cutoff := r.d.Now().Add(-r.o.UploadMaxAge).UnixMilli()
	ups, err := r.d.DB.ExpiredUploads(ctx, cutoff, gcBatch)
	if err != nil {
		return err
	}
	for _, u := range ups {
		if err := r.propose(ctx, &meta.AbortUpload{UploadID: u.ID, Now: r.d.Now().UnixMilli()}); err != nil {
			return err
		}
		r.update(func(s *Stats) { s.UploadsAborted++ })
	}
	return nil
}
