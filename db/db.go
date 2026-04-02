package db

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/lib/pq"
)

func Connect(databaseURL string) (*sql.DB, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("db open: %w", err)
	}

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("db ping: %w", err)
	}

	return db, nil
}

// RunMigrations applies the schema. Safe to call on every startup
// because all statements use IF NOT EXISTS.
func RunMigrations(db *sql.DB) error {
	schema, err := os.ReadFile("db/schema.sql")
	if err != nil {
		return fmt.Errorf("read schema: %w", err)
	}
	_, err = db.Exec(string(schema))
	if err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}
