package session

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func NewHandler(next http.Handler, db *pgxpool.Pool) (h http.Handler, err error) {
	_, err = db.Exec(context.Background(), "CREATE TABLE IF NOT EXISTS sessions(token bytea primary key)")
	if err != nil {
		return
	}

	_, err = db.Exec(context.Background(), "CREATE TABLE IF NOT EXISTS sessions_kv(token bytea references sessions, key text, value text not null, primary key (token, key))")
	if err != nil {
		return
	}

	h = &sessionHandler{db, next}
	return
}

type sessionHandler struct {
	db   *pgxpool.Pool
	next http.Handler
}

func existingSessionForTok(ctx context.Context, tx pgx.Tx, k [32]byte) (s *Session, err error) {
	var exists bool
	err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT * FROM sessions WHERE token = $1)", k[:]).Scan(&exists)
	if err != nil {
		return
	}
	if !exists {
		err = errors.New("no matching session for token")
		return
	}

	rs, err := tx.Query(ctx, "SELECT key, value FROM sessions_kv WHERE token = $1", k[:])
	if err != nil {
		return
	}
	defer rs.Close()

	orig := make(map[string]string)
	for rs.Next() {
		var (
			k string
			v string
		)

		err = rs.Scan(&k, &v)
		if err != nil {
			return
		}

		orig[k] = v
	}

	s = &Session{k, orig, nil}
	return
}

func sessionForReq(ctx context.Context, tx pgx.Tx, w http.ResponseWriter, r *http.Request) (s *Session, err error) {
	var k [32]byte

	var c *http.Cookie
	if c, err = r.Cookie("session"); err == nil {
		var k2 []byte
		if k2, err = base64.RawStdEncoding.DecodeString(c.Value); err == nil {
			if len(k2) == len(k) {
				copy(k[:], k2)
				if s, err = existingSessionForTok(ctx, tx, k); err == nil {
					return
				}
			}
		}
	}

	binary.BigEndian.PutUint64(k[:8], uint64(time.Now().UnixNano()))
	_, err = rand.Read(k[8:])

	if err != nil {
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    base64.RawStdEncoding.EncodeToString(k[:]),
		Path:     "/",
		SameSite: http.SameSiteStrictMode,
		HttpOnly: true,
	})

	s = &Session{k, nil, nil}
	return
}

type sessionKey struct{}

func GetSession(ctx context.Context) (s *Session) {
	s, _ = ctx.Value(sessionKey{}).(*Session)
	return
}

func (h *sessionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	tx, err := h.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		w.WriteHeader(500)
		io.WriteString(w, err.Error())
		return
	}
	defer tx.Rollback(ctx)

	s, err := sessionForReq(ctx, tx, w, r)
	if err != nil {
		w.WriteHeader(500)
		io.WriteString(w, err.Error())
		return
	}

	h.next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, sessionKey{}, s)))

	if err = s.write(ctx, tx); err != nil {
		fmt.Println("session:", err)
		return
	}

	if err = tx.Commit(ctx); err != nil {
		fmt.Println("session:", err)
	}
}

type Session struct {
	token [32]byte
	orig  map[string]string
	addl  map[string]string
}

func (s *Session) Get(k string) (v string, ok bool) {
	if s.addl != nil {
		v, ok = s.addl[k]
	}
	if !ok && s.orig != nil {
		v, ok = s.orig[k]
	}
	return
}

func (s *Session) Set(k string, v string) {
	if s.addl == nil {
		s.addl = make(map[string]string)
	}
	s.addl[k] = v
}

func (s *Session) write(ctx context.Context, tx pgx.Tx) (err error) {
	if s.addl == nil {
		return
	}

	if s.orig == nil {
		_, err = tx.Exec(ctx, "INSERT INTO sessions VALUES($1)", s.token[:])
		if err != nil {
			return
		}
	}

	var b pgx.Batch
	for k, v := range s.addl {
		b.Queue(
			"INSERT INTO sessions_kv VALUES ($1, $2, $3) ON CONFLICT ON CONSTRAINT sessions_kv_pkey DO UPDATE SET value = $3 WHERE sessions_kv.token = $1 AND sessions_kv.key = $2",
			s.token[:],
			k,
			v,
		)
	}

	err = tx.SendBatch(ctx, &b).Close()

	return
}
