// Package receipt handles inbound receipt images: validation, hashing and
// storage-key layout. Extraction lives in internal/extraction.
package receipt

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/extraction/mock"
)

// allowedTypes are the image content types accepted from the provider.
var allowedTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/webp": true,
}

// ValidateAndHash reads at most maxBytes from r, verifies the content is a
// supported receipt image, and returns the bytes with their sha256 hex digest
// and sniffed content type.
//
// Dev convenience: text beginning with mock.MockPrefix is accepted as
// "text/plain" so the simulate harness can drive the pipeline without real
// images. Everything else that is not JPEG/PNG/WEBP is CodeValidation.
func ValidateAndHash(r io.Reader, maxBytes int64) (data []byte, sha256Hex string, contentType string, err error) {
	data, err = io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, "", "", domain.E(domain.CodeTransient, "reading image bytes", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, "", "", domain.Ef(domain.CodeValidation, nil, "image exceeds %d bytes", maxBytes)
	}
	if len(data) == 0 {
		return nil, "", "", domain.E(domain.CodeValidation, "empty image", nil)
	}

	if strings.HasPrefix(string(data), mock.MockPrefix) {
		contentType = "text/plain"
	} else {
		sniffed, _, _ := strings.Cut(http.DetectContentType(data), ";")
		if !allowedTypes[sniffed] {
			return nil, "", "", domain.Ef(domain.CodeValidation, nil, "unsupported image type %q", sniffed)
		}
		contentType = sniffed
	}

	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), contentType, nil
}

// StorageKey is the object-store layout for originals: receipts/{user}/{receipt}.
func StorageKey(userID, receiptID uuid.UUID) string {
	return fmt.Sprintf("receipts/%s/%s", userID, receiptID)
}
