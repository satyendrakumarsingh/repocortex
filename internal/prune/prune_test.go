package prune

import (
	"strings"
	"testing"
)

func TestPruneGoFile(t *testing.T) {
	input := `package service

import "context"

// UserService handles account operations.
type UserService struct {
	repo Repository
}

// User represents an account holder.
type User struct {
	ID   string
	Name string
}

// GetUser retrieves a user by ID.
func (s *UserService) GetUser(ctx context.Context, id string) (*User, error) {
	if id == "" {
		return nil, errors.New("empty id")
	}
	user, err := s.repo.Find(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to find: %w", err)
	}
	log.Printf("found user %s", user.Name)
	return user, nil
}

// helper is a private utility.
func helper(x int) int {
	y := x * 2
	return y + 1
}
`

	got, err := PruneFile("service.go", []byte(input))
	if err != nil {
		t.Fatalf("PruneFile failed: %v", err)
	}

	result := string(got)

	// Verify declarations and docstrings are kept
	if !strings.Contains(result, "type UserService struct {") {
		t.Errorf("missing UserService struct definition")
	}
	if !strings.Contains(result, "type User struct {") {
		t.Errorf("missing User struct definition")
	}
	if !strings.Contains(result, "// GetUser retrieves a user by ID.") {
		t.Errorf("missing docstring for GetUser")
	}
	if !strings.Contains(result, "func (s *UserService) GetUser(ctx context.Context, id string) (*User, error) { /* ... */ }") {
		t.Errorf("GetUser signature not pruned properly:\n%s", result)
	}
	if !strings.Contains(result, "func helper(x int) int { /* ... */ }") {
		t.Errorf("helper signature not pruned properly:\n%s", result)
	}

	// Verify implementation details are stripped
	if strings.Contains(result, "log.Printf") || strings.Contains(result, "errors.New") {
		t.Errorf("implementation body was not stripped:\n%s", result)
	}
}

func TestPrunePythonFile(t *testing.T) {
	input := `import os
from typing import Optional

class UserService:
    """UserService manages accounts."""

    def __init__(self, db_url: str):
        """Initialize the service."""
        self.db = connect(db_url)
        self.cache = {}

    def get_user(self, user_id: str) -> Optional[dict]:
        """Fetch a user by ID."""
        if not user_id:
            raise ValueError("Invalid user_id")
        return self.db.query("SELECT * FROM users WHERE id = %s", user_id)

def calculate_tax(amount: float) -> float:
    rate = 0.08
    return amount * rate
`

	got, err := PruneFile("service.py", []byte(input))
	if err != nil {
		t.Fatalf("PruneFile failed: %v", err)
	}

	result := string(got)

	if !strings.Contains(result, "class UserService:") {
		t.Errorf("missing UserService class header")
	}
	if !strings.Contains(result, `"""UserService manages accounts."""`) {
		t.Errorf("missing class docstring")
	}
	if !strings.Contains(result, `"""Fetch a user by ID."""`) {
		t.Errorf("missing method docstring")
	}
	if !strings.Contains(result, "def get_user(self, user_id: str) -> Optional[dict]:") {
		t.Errorf("missing get_user signature")
	}
	if !strings.Contains(result, "def calculate_tax(amount: float) -> float:") {
		t.Errorf("missing calculate_tax signature")
	}

	// Verify bodies stripped
	if strings.Contains(result, "SELECT * FROM users") || strings.Contains(result, "rate = 0.08") {
		t.Errorf("Python implementation body was not stripped:\n%s", result)
	}
}

func TestPruneTypeScriptFile(t *testing.T) {
	input := `import { Logger } from './logger';

export interface User {
  id: string;
  email: string;
}

export class AuthController {
  private logger: Logger;

  /** Authenticate a user session */
  public async login(req: Request, res: Response): Promise<void> {
    const { email, password } = req.body;
    const isValid = await verifyPassword(password);
    if (!isValid) {
      res.status(401).json({ error: 'unauthorized' });
      return;
    }
    res.json({ token: 'jwt-123' });
  }
}
`

	got, err := PruneFile("controller.ts", []byte(input))
	if err != nil {
		t.Fatalf("PruneFile failed: %v", err)
	}

	result := string(got)

	if !strings.Contains(result, "export interface User {") {
		t.Errorf("missing interface User")
	}
	if !strings.Contains(result, "export class AuthController {") {
		t.Errorf("missing class AuthController")
	}
	if !strings.Contains(result, "/** Authenticate a user session */") {
		t.Errorf("missing TS doc comment")
	}
	if !strings.Contains(result, "public async login(req: Request, res: Response): Promise<void> { /* ... */ }") {
		t.Errorf("login method not pruned properly:\n%s", result)
	}
	if strings.Contains(result, "verifyPassword") || strings.Contains(result, "res.status(401)") {
		t.Errorf("TS implementation body was not stripped:\n%s", result)
	}
}
