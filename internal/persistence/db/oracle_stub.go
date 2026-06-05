//go:build !oracle

package db

import (
	"fmt"

	"gorm.io/gorm"
)

// oracleDialect is a stub when the daemon is built without Oracle support.
// To enable Oracle:
//  1. Install Oracle Instant Client on the host
//  2. Add: go get github.com/oracle-samples/gorm-oracle
//  3. Build with: go build -tags oracle ./...
func oracleDialect(dsn string) (gorm.Dialector, error) {
	return nil, fmt.Errorf("db: oracle driver not built — rebuild with -tags oracle and install Oracle Instant Client")
}
