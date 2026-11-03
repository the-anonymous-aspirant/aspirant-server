package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// PDF text extraction for the N26 import (system_3 #9812). The text layer is
// read with poppler's pdftotext (-layout keeps the template's two-column
// label/value rows on one line), run as a subprocess so a malformed PDF can
// at worst kill the child, never the server. The pure-Go candidates could not
// decode these files' font cmaps at all — the evaluation is in
// docs/DECISIONS.md ("PDF text extraction for the N26 import").

const (
	// n26ExtractTimeout bounds one pdftotext run; the files are one or a few
	// pages, so a run that needs longer is stuck, not large.
	n26ExtractTimeout = 10 * time.Second

	// maxN26PDFBytes bounds one uploaded PDF, same cap as the PayPal CSV
	// import. The real confirmations are ~180 KB.
	maxN26PDFBytes = 5 << 20

	// maxN26FilesPerRequest bounds one parse request; the operator's batch is
	// five files, and a bigger upload is better split anyway.
	maxN26FilesPerRequest = 10
)

var errNotAPDF = errors.New("the file is not a PDF")

// ExtractN26PDFText returns the PDF's text layer via `pdftotext -layout`.
// pdftotext needs a seekable input, so the bytes go through a temp file that
// is removed before returning.
func ExtractN26PDFText(data []byte) (string, error) {
	if !bytes.HasPrefix(data, []byte("%PDF-")) {
		return "", errNotAPDF
	}
	tmp, err := os.CreateTemp("", "n26-import-*.pdf")
	if err != nil {
		return "", fmt.Errorf("temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", fmt.Errorf("temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("temp file: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), n26ExtractTimeout)
	defer cancel()
	var out, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "pdftotext", "-layout", tmp.Name(), "-")
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("pdftotext timed out after %s", n26ExtractTimeout)
		}
		return "", fmt.Errorf("pdftotext: %v (%s)", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return out.String(), nil
}
