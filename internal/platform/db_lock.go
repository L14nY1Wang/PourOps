package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const migrationLockTimeout = 30 * time.Second

func acquireMigrationLock(ctx context.Context, conn *pgxpool.Conn) error {
	deadline := time.Now().Add(migrationLockTimeout)
	for {
		var acquired bool
		if err := conn.QueryRow(ctx, `select pg_try_advisory_lock($1)`, MigrationLockKey).Scan(&acquired); err != nil {
			return err
		}
		if acquired {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待数据库迁移锁超时")
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func sqlChecksum(sqlText string) string {
	sum := sha256.Sum256([]byte(sqlText))
	return hex.EncodeToString(sum[:])
}
