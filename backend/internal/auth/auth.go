package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

var (
	mu          sync.Mutex
	tokens      = map[string]time.Time{}
	sweeperOnce sync.Once
)

// ── login rate limiting (per-IP, fixed 1-minute window) ──

const (
	loginWindow      = time.Minute
	loginMaxFailures = 10
)

type rlState struct {
	count     int
	windowEnd time.Time
}

var (
	loginMu sync.Mutex
	logins  = map[string]*rlState{}
)

// Allow reports whether ip may attempt a login right now. An expired window is
// treated as a fresh start.
func Allow(ip string) bool {
	loginMu.Lock()
	defer loginMu.Unlock()
	st := logins[ip]
	if st == nil || time.Now().After(st.windowEnd) {
		return true
	}
	return st.count < loginMaxFailures
}

// RecordFail increments the failure counter for ip, opening a fresh window if
// the previous one has elapsed.
func RecordFail(ip string) {
	loginMu.Lock()
	defer loginMu.Unlock()
	now := time.Now()
	st := logins[ip]
	if st == nil || now.After(st.windowEnd) {
		st = &rlState{count: 0, windowEnd: now.Add(loginWindow)}
		logins[ip] = st
	}
	st.count++
}

// Reset clears the failure counter for ip (called on a successful login).
func Reset(ip string) {
	loginMu.Lock()
	delete(logins, ip)
	loginMu.Unlock()
}

// cleanup removes expired tokens. Caller must NOT hold mu.
func cleanup() {
	mu.Lock()
	defer mu.Unlock()
	now := time.Now()
	for t, exp := range tokens {
		if !now.Before(exp) {
			delete(tokens, t)
		}
	}
}

// startSweeper launches a background goroutine that prunes expired tokens
// every 10 minutes, so tokens from abandoned sessions don't accumulate
// forever. Started once on first token issuance.
func startSweeper() {
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for range t.C {
			cleanup()
		}
	}()
}

func HashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	return string(b), err
}

func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func NewToken() string {
	sweeperOnce.Do(startSweeper)
	b := make([]byte, 16)
	rand.Read(b) //nolint
	t := hex.EncodeToString(b)
	mu.Lock()
	tokens[t] = time.Now().Add(24 * time.Hour)
	mu.Unlock()
	return t
}

func Valid(token string) bool {
	mu.Lock()
	defer mu.Unlock()
	exp, ok := tokens[token]
	if !ok {
		return false
	}
	if time.Now().Before(exp) {
		return true
	}
	delete(tokens, token) // expired — evict on access too
	return false
}

// Middleware protects admin routes with a Bearer session token.
// If passwordHash is empty, auth is disabled (backward-compat).
func Middleware(passwordHash string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if passwordHash == "" {
			c.Next()
			return
		}
		token := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		if !Valid(token) {
			c.JSON(401, gin.H{"error": "unauthorized"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// GenerateAPIKey returns a fresh random proxy API key: sk-bb-<32 hex chars>.
func GenerateAPIKey() string {
	b := make([]byte, 16)
	rand.Read(b) //nolint
	return "sk-bb-" + hex.EncodeToString(b)
}

// extractAPIKey pulls the client-supplied key from either the Anthropic
// convention (x-api-key) or the OpenAI convention (Authorization: Bearer).
func extractAPIKey(c *gin.Context) string {
	if k := c.GetHeader("x-api-key"); k != "" {
		return k
	}
	return strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
}

// CheckAPIKey validates the request against the unified proxy API key.
// Returns true when the request may proceed. When expected is empty, auth is
// disabled (backward compatibility). On failure it aborts with a 401 whose
// body is OpenAI-style so both OpenAI and Anthropic SDKs recognize it.
func CheckAPIKey(c *gin.Context, expected string) bool {
	if expected == "" {
		return true
	}
	provided := extractAPIKey(c)
	if subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
		c.AbortWithStatusJSON(401, gin.H{
			"error": gin.H{
				"message": "invalid api key",
				"type":    "authentication_error",
			},
		})
		return false
	}
	return true
}
