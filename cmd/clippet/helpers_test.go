package main

import (
	"clippet/internal/config"
	"clippet/internal/db"
)

func openForTest(cfg config.Config) (*db.DB, error) {
	return db.Open(dbPath(cfg))
}
