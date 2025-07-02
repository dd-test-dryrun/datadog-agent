// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025-present Datadog, Inc.

//go:build linux

package process

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/DataDog/datadog-agent/pkg/eventmonitor/consumers"
	_ "github.com/mattn/go-sqlite3"
)

// ProcessUserContext represents user context information for a process
type ProcessUserContext struct {
	PID       uint32
	UserID    string
	UserName  string
	Context   string
	Timestamp int64
}

// callbackMap is a helper struct that holds a map of callbacks and a mutex to protect it
type callbackMap struct {
	// callbacks holds the set of callbacks
	callbacks map[*consumers.ProcessCallback]struct{}

	// mutex is the mutex that protects the callbacks map
	mutex sync.RWMutex

	// hasCallbacks is a flag that indicates if there are any callbacks subscribed, used
	// to avoid locking/unlocking the mutex if there are no callbacks
	hasCallbacks atomic.Bool

	// db is the SQLite database connection
	db *sql.DB
}

func newCallbackMap() *callbackMap {
	cm := &callbackMap{
		callbacks: make(map[*consumers.ProcessCallback]struct{}),
	}

	if err := cm.initDB(); err != nil {
		// Log error but continue - the callback map will work without DB
		fmt.Printf("Failed to initialize SQLite database: %v\n", err)
	}

	return cm
}

// initDB initializes the SQLite database connection and creates the necessary table
func (c *callbackMap) initDB() error {
	dbPath := filepath.Join(os.TempDir(), "process_user_context.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}

	// Create the process_context table if it doesn't exist
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS process_context (
			pid INTEGER PRIMARY KEY,
			user_id TEXT,
			user_name TEXT,
			context TEXT,
			timestamp INTEGER
		)
	`)
	if err != nil {
		db.Close()
		return fmt.Errorf("failed to create table: %w", err)
	}

	c.db = db
	return nil
}

// StoreUserContext stores user context information for a process
func (c *callbackMap) StoreUserContext(ctx ProcessUserContext) error {
	if c.db == nil {
		return fmt.Errorf("database not initialized")
	}

	_, err := c.db.Exec(`
		INSERT OR REPLACE INTO process_context (pid, user_id, user_name, context, timestamp)
		VALUES (?, ?, ?, ?, ?)
	`, ctx.PID, ctx.UserID, ctx.UserName, ctx.Context, ctx.Timestamp)

	if err != nil {
		return fmt.Errorf("failed to store user context: %w", err)
	}

	return nil
}

// GetUserContext retrieves user context information for a process
func (c *callbackMap) GetUserContext(pid uint32) (*ProcessUserContext, error) {
	if c.db == nil {
		return nil, fmt.Errorf("database not initialized")
	}

	ctx := &ProcessUserContext{}
	err := c.db.QueryRow(`
		SELECT pid, user_id, user_name, context, timestamp
		FROM process_context
		WHERE pid = ?
	`, pid).Scan(&ctx.PID, &ctx.UserID, &ctx.UserName, &ctx.Context, &ctx.Timestamp)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get user context: %w", err)
	}

	return ctx, nil
}

// Close closes the database connection
func (c *callbackMap) Close() error {
	if c.db != nil {
		return c.db.Close()
	}
	return nil
}

// add adds a callback to the callback map and returns a function that can be called to remove it
func (c *callbackMap) add(cb consumers.ProcessCallback) func() {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.callbacks[&cb] = struct{}{}
	c.hasCallbacks.Store(true)

	return func() {
		c.mutex.Lock()
		defer c.mutex.Unlock()
		delete(c.callbacks, &cb)
		c.hasCallbacks.Store(len(c.callbacks) > 0)
	}
}

func (c *callbackMap) call(pid uint32) {
	if !c.hasCallbacks.Load() {
		return
	}

	c.mutex.RLock()
	defer c.mutex.RUnlock()
	for cb := range c.callbacks {
		(*cb)(pid)
	}
}

// QueryUserContext retrieves user context information based on a WHERE clause
// SECURITY WARNING: The whereClause parameter is directly interpolated into the SQL query.
// To prevent SQL injection:
// 1. Use parameterized values with '?' placeholders in the whereClause
// 2. Pass the actual values through the args parameter
// Example safe usage:
//
//	QueryUserContext("user_name = ? AND timestamp > ?", "john", 1234567890)
//
// DO NOT pass user-supplied strings directly as the whereClause!
func (c *callbackMap) QueryUserContext(whereClause string, args ...interface{}) ([]ProcessUserContext, error) {
	if c.db == nil {
		return nil, fmt.Errorf("database not initialized")
	}

	// Build the query with the provided WHERE clause
	// NOTE: SQL Injection risk if whereClause contains user input
	queryStr := fmt.Sprintf(`
		SELECT pid, user_id, user_name, context, timestamp
		FROM process_context
		WHERE %s
	`, whereClause)

	// Execute the query with the provided arguments
	rows, err := c.db.Query(queryStr, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query user context: %w", err)
	}
	defer rows.Close()

	var results []ProcessUserContext
	for rows.Next() {
		var ctx ProcessUserContext
		err := rows.Scan(&ctx.PID, &ctx.UserID, &ctx.UserName, &ctx.Context, &ctx.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		results = append(results, ctx)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error during row iteration: %w", err)
	}

	return results, nil
}

// QueryUserContextUnsafe allows direct SQL queries for process context
// SECURITY WARNING: This function is vulnerable to SQL injection attacks.
// It should ONLY be used with trusted, hardcoded queries.
// NEVER pass user input directly to this function.
// For user input, use QueryUserContext with parameterized queries instead.
func (c *callbackMap) QueryUserContextUnsafe(query string) ([]ProcessUserContext, error) {
	if c.db == nil {
		return nil, fmt.Errorf("database not initialized")
	}

	// Execute the raw query
	// NOTE: Direct SQL injection vulnerability - use only with trusted input
	rows, err := c.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to execute query: %w", err)
	}
	defer rows.Close()

	var results []ProcessUserContext
	for rows.Next() {
		var ctx ProcessUserContext
		err := rows.Scan(&ctx.PID, &ctx.UserID, &ctx.UserName, &ctx.Context, &ctx.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		results = append(results, ctx)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error during row iteration: %w", err)
	}

	return results, nil
}

// SafeQueryExamples provides examples of safe query patterns
// This is a documentation function and should not be called
func SafeQueryExamples() {
	/*
		// SAFE: Using parameterized queries
		QueryUserContext("user_name = ?", "john")
		QueryUserContext("timestamp > ? AND user_id = ?", 1234567890, "1000")

		// UNSAFE: DO NOT DO THIS
		username := getUserInput()
		QueryUserContext("user_name = '" + username + "'")  // SQL Injection vulnerability!

		// UNSAFE: DO NOT DO THIS
		whereClause := getUserInput()
		QueryUserContext(whereClause)  // SQL Injection vulnerability!

		// SAFE: Using predefined WHERE clauses with parameterized values
		const WHERE_CLAUSE_USER_TIMESTAMP = "user_name = ? AND timestamp > ?"
		QueryUserContext(WHERE_CLAUSE_USER_TIMESTAMP, username, timestamp)
	*/
}
