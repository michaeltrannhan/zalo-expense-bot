package account

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/platform/objectstore"
	"zl-expese-bot/internal/store"
)

// Report tallies the user-linked material removed by DeleteAccount.
type Report struct {
	ReceiptsDeleted     int `json:"receipts_deleted"`
	TransactionsDeleted int `json:"transactions_deleted"`
	FilesRemoved        int `json:"files_removed"`
	ExportsRemoved      int `json:"exports_removed"`
	RowsPurged          int `json:"rows_purged"`
	JobsPurged          int `json:"jobs_purged"`
}

// DeleteAccount executes the data-deletion right for userID.
//
// Every application path that can mutate user data participates in the same
// advisory lock. Deletion therefore waits for in-flight work, purges queued
// work, and prevents a worker from recreating rows after commit. The users
// row remains only as a de-identified lifecycle tombstone; all provider IDs,
// payloads, receipts, derived records, analytics, message copies and local
// CSV/JSON export artifacts are physically removed.
//
// The saga is durable across retries:
//  1. Tx1 marks status=deleting and lists object keys (FOR UPDATE).
//  2. Outside the DB: delete objects and export artifacts (idempotent).
//  3. Tx2 purges all user-linked rows and finalises the deleted tombstone.
//
// If already deleting on entry, Tx1 is skipped past the status flip and the
// saga resumes at object cleanup + Tx2.
func DeleteAccount(ctx context.Context, st *store.Store, objects objectstore.Store, dataDir string, userID, triggeringMessageID uuid.UUID) (Report, error) {
	var report Report
	err := st.WithUserLock(ctx, userID, func(lockedCtx context.Context) error {
		var err error
		report, err = deleteAccountLocked(lockedCtx, st, objects, dataDir, userID, triggeringMessageID)
		return err
	})
	return report, err
}

func deleteAccountLocked(ctx context.Context, st *store.Store, objects objectstore.Store, dataDir string, userID, triggeringMessageID uuid.UUID) (Report, error) {
	var report Report

	keys, err := st.BeginAccountDeletion(ctx, userID)
	if err != nil {
		return report, asDomain(err, "delete account mark deleting")
	}

	if len(keys) > 0 && objects == nil {
		return report, domain.E(domain.CodeTransient, "receipt object store unavailable during account deletion", nil)
	}
	for _, key := range keys {
		if err := objects.Delete(ctx, key); err != nil {
			return report, domain.Ef(domain.CodeTransient, err, "delete receipt object %s", key)
		}
		report.FilesRemoved++
	}
	exportsRemoved, err := removeExportArtifacts(dataDir, userID)
	if err != nil {
		return report, domain.E(domain.CodeTransient, "delete account exports", err)
	}
	report.ExportsRemoved = exportsRemoved

	stats, err := st.PurgeAccountData(ctx, userID, triggeringMessageID)
	if err != nil {
		return report, asDomain(err, "delete account purge")
	}
	report.ReceiptsDeleted = stats.ReceiptsDeleted
	report.TransactionsDeleted = stats.TransactionsDeleted
	report.RowsPurged = stats.RowsPurged
	report.JobsPurged = stats.JobsPurged
	return report, nil
}

func asDomain(err error, fallback string) error {
	var de *domain.Error
	if errors.As(err, &de) {
		return de
	}
	return domain.E(domain.CodeTransient, fallback, err)
}

func removeExportArtifacts(dataDir string, userID uuid.UUID) (int, error) {
	if strings.TrimSpace(dataDir) == "" {
		return 0, nil
	}
	root, err := filepath.Abs(filepath.Join(dataDir, "exports"))
	if err != nil {
		return 0, fmt.Errorf("resolve export root: %w", err)
	}
	target := filepath.Join(root, userID.String())
	rel, err := filepath.Rel(root, target)
	if err != nil || rel != userID.String() {
		return 0, fmt.Errorf("refuse unsafe export path %q", target)
	}
	entries, err := os.ReadDir(target)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read export directory: %w", err)
	}
	if err := os.RemoveAll(target); err != nil {
		return 0, fmt.Errorf("remove export directory: %w", err)
	}
	return len(entries), nil
}
