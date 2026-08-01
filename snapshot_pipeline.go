package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/aidenappl/lattice-runner/backup"
)

// snapshotUploadBuffer sits between the compressor and the uploader.
//
// io.Pipe has no internal buffering, so without this every hesitation in the
// upload applies backpressure straight through the compressor to the dump's
// stdout — while the dump holds its read snapshot open inside the database. A few
// part-sizes of slack absorbs ordinary network jitter.
const snapshotUploadBuffer = 32 * 1024 * 1024

// streamSnapshot dumps a database and writes it to a backup destination without
// staging the whole thing anywhere.
//
// Returns the number of bytes stored (compressed).
//
// The failure semantics are the point of this function. A dump that dies partway
// must not produce a stored object: ExecDatabaseDump surfaces a non-zero exit as
// a read error, that error propagates through the compressor to the uploader, and
// a streaming uploader aborts its multipart upload rather than completing a
// truncated object. The shell equivalent of this pipeline does the opposite —
// gzip compresses the partial dump and exits 0, producing a perfectly valid
// archive of a broken backup, which is a documented MySQL bug (#50272) and the
// mechanism behind more than one public data-loss postmortem.
func streamSnapshot(
	ctx context.Context,
	dumper func(context.Context) (io.ReadCloser, error),
	dest backup.Destination,
	remotePath string,
) (int64, error) {
	dump, err := dumper(ctx)
	if err != nil {
		return 0, err
	}
	defer dump.Close()

	streamer, canStream := dest.(backup.StreamUploader)
	if !canStream {
		// Google Drive and Samba want something file-shaped. Stage to a temp
		// file, but still stream *into* it, so the runner's memory is never the
		// constraint even when local disk is.
		return stageAndUpload(ctx, dump, dest, remotePath)
	}

	pr, pw := io.Pipe()

	go func() {
		buffered := bufio.NewWriterSize(pw, snapshotUploadBuffer)
		gz := gzip.NewWriter(buffered)

		_, copyErr := io.Copy(gz, dump)
		if copyErr != nil {
			// CloseWithError, never Close: the uploader must see a failure and
			// abort, not a clean EOF that completes a truncated object.
			pw.CloseWithError(fmt.Errorf("dump stream failed: %w", copyErr))
			return
		}

		// Close in order and check both: gzip writes its trailer on Close, and a
		// missing trailer is a corrupt archive that only fails at restore time.
		if err := gz.Close(); err != nil {
			pw.CloseWithError(fmt.Errorf("finalise compression: %w", err))
			return
		}
		if err := buffered.Flush(); err != nil {
			pw.CloseWithError(fmt.Errorf("flush upload buffer: %w", err))
			return
		}
		pw.Close()
	}()

	size, err := streamer.UploadStream(ctx, pr, remotePath)
	if err != nil {
		// Unblock the writer if the upload died first, or it leaks holding the
		// database's dump open.
		pr.CloseWithError(err)
		return 0, err
	}
	return size, nil
}

// stageAndUpload writes the compressed dump to a temp file, then uploads it.
// Used for destinations that cannot consume a stream.
func stageAndUpload(ctx context.Context, dump io.Reader, dest backup.Destination, remotePath string) (int64, error) {
	tmpDir, err := os.MkdirTemp("", "lattice-snapshot-*")
	if err != nil {
		return 0, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	tmpFile := filepath.Join(tmpDir, "dump.sql.gz")
	f, err := os.Create(tmpFile)
	if err != nil {
		return 0, fmt.Errorf("create temp file: %w", err)
	}

	buffered := bufio.NewWriterSize(f, 4*1024*1024)
	gz := gzip.NewWriter(buffered)

	if _, err := io.Copy(gz, dump); err != nil {
		gz.Close()
		f.Close()
		return 0, fmt.Errorf("dump stream failed: %w", err)
	}
	if err := gz.Close(); err != nil {
		f.Close()
		return 0, fmt.Errorf("finalise compression: %w", err)
	}
	if err := buffered.Flush(); err != nil {
		f.Close()
		return 0, fmt.Errorf("flush staged dump: %w", err)
	}
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("close staged dump: %w", err)
	}

	return dest.Upload(ctx, tmpFile, remotePath)
}
