package accountmanagement

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/google/uuid"
)

const MaxPDFBytes = 5 << 20

func invoiceRoot(authDir string, create bool) (*os.Root, error) {
	path := filepath.Join(authDir, ".account-invoices")
	if create {
		if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
			return nil, err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("invalid invoice directory")
	}
	if create {
		if err := os.Chmod(path, 0700); err != nil {
			return nil, err
		}
	}
	return os.OpenRoot(path)
}

func ValidatePDF(data []byte, digest string) error {
	if len(data) == 0 || len(data) > MaxPDFBytes || !bytes.HasPrefix(data, []byte("%PDF-")) {
		return fmt.Errorf("invalid_pdf")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != digest {
		return fmt.Errorf("pdf_digest_mismatch")
	}
	return nil
}

func StorePDF(authDir, digest string, data []byte) error {
	if err := ValidatePDF(data, digest); err != nil {
		return err
	}
	root, err := invoiceRoot(authDir, true)
	if err != nil {
		return err
	}
	defer root.Close()
	name := digest + ".pdf"
	if _, err := root.Stat(name); err == nil {
		_, err = readPDF(root, digest)
		return err
	} else if !os.IsNotExist(err) {
		return err
	}
	tmp := ".invoice-" + uuid.NewString()
	file, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = root.Rename(tmp, name); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func ReadPDF(authDir, digest string) ([]byte, error) {
	if len(digest) != 64 {
		return nil, fmt.Errorf("invalid invoice reference")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return nil, fmt.Errorf("invalid invoice reference")
	}
	root, err := invoiceRoot(authDir, false)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return readPDF(root, digest)
}

func readPDF(root *os.Root, digest string) ([]byte, error) {
	file, err := root.Open(digest + ".pdf")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxPDFBytes+1))
	if err != nil {
		return nil, err
	}
	if err := ValidatePDF(data, digest); err != nil {
		return nil, err
	}
	return data, nil
}
