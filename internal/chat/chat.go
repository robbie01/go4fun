package chat

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"sohio.net/go4fun/internal/broadcast"
	"sohio.net/go4fun/internal/session"
	"sohio.net/go4fun/internal/snowflake"
)

//go:embed chat.html
var st string

var t *template.Template
var u websocket.Upgrader

func init() {
	var err error
	t, err = template.New("root").Parse(st)

	if err != nil {
		log.Fatalln("chat mod init:", err)
	}
}

type chatHandler struct {
	*http.ServeMux
	db *pgxpool.Pool
	cs *broadcast.Set[string]
}

type message struct {
	Id       int64
	Username string
	Msg      string
}

func NewHandler(db *pgxpool.Pool, cs *broadcast.Set[string]) (h http.Handler, err error) {
	_, err = db.Exec(context.Background(), "CREATE TABLE IF NOT EXISTS messages(id bigint primary key, username text not null, msg text not null)")
	if err != nil {
		return
	}

	innerH := &chatHandler{http.NewServeMux(), db, cs}

	innerH.ServeMux.HandleFunc("GET /chat/{$}", innerH.serveRoot)
	innerH.ServeMux.HandleFunc("POST /chat/send", innerH.serveSend)
	innerH.ServeMux.HandleFunc("/chat/ws", innerH.serveWs)

	return innerH, nil
}

func (h *chatHandler) serveRoot(w http.ResponseWriter, r *http.Request) {
	type channel struct {
		Name string
		Href string
	}

	data := struct {
		Channels   []channel
		Ws         string
		LiveUpdate bool
		Messages   []message
	}{
		Channels: []channel{{Name: "general"}},
		Ws:       r.URL.JoinPath("ws").EscapedPath(),
	}

	s := session.GetSession(r.Context())
	if _, ok := s.Get("username"); !ok {
		s.Set("username", "robbie")
	}

	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Expires", "0")

	rs, err := h.db.Query(r.Context(), "SELECT id, username, msg FROM messages")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, err)
	}
	defer rs.Close()

	for rs.Next() {
		var msg message
		if err := rs.Scan(&msg.Id, &msg.Username, &msg.Msg); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, err)
			return
		}
		data.Messages = append(data.Messages, msg)
	}

	w.Header().Set("Content-Type", "text/html; utf-8")
	w.WriteHeader(http.StatusOK)

	if err := t.Execute(w, data); err != nil {
		fmt.Println("chat:", err)
	}
}

func (h *chatHandler) serveWs(w http.ResponseWriter, r *http.Request) {
	conn, err := u.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	var initial struct {
		After string `json:"after"`
	}
	if err := conn.ReadJSON(&initial); err != nil {
		conn.Close()
		return
	}

	ch := h.cs.MakeChan()

	ctx := context.Background()

	// Handle received messages and close
	go func() {
		defer ch.Close()

		for {
			_, r, err := conn.NextReader()
			if err != nil {
				break
			}
			io.Copy(io.Discard, r)
		}
	}()

	// Listen for notifications from broadcast set
	go func() {
		defer conn.Close()

		last, err := strconv.ParseInt(initial.After, 10, 64)
		if err != nil {
			last = -1
		}

		f := func() error {
			var msgs []message

			rs, err := h.db.Query(ctx, "SELECT id, username, msg FROM messages WHERE id > $1", last)
			if err != nil {
				return err
			}
			defer rs.Close()

			for rs.Next() {
				var msg message
				if err := rs.Scan(&msg.Id, &msg.Username, &msg.Msg); err != nil {
					return err
				}
				last = msg.Id
				msgs = append(msgs, msg)
			}
			if len(msgs) == 0 {
				return nil
			}
			rs.Close()

			w, err := conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return err
			}
			defer w.Close()

			data := struct {
				LiveUpdate bool
				Messages   []message
			}{true, msgs}

			if err := t.ExecuteTemplate(w, "messages", data); err != nil {
				return err
			}

			return nil
		}

		if err := f(); err != nil {
			conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, err.Error()))
			return
		}
		for range ch.Receiver() {
			if err := f(); err != nil {
				conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, err.Error()))
				return
			}
		}
	}()
}

func (h *chatHandler) serveSend(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Expires", "0")

	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, err)
		return
	}

	msg := strings.TrimSpace(r.PostFormValue("msg"))
	username, ok := session.GetSession(r.Context()).Get("username")
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "no username for session")
		return
	}

	if msg != "" {
		for {
			tx, err := h.db.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.Serializable})
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, err)
				return
			}

			var id int64
			if err := tx.QueryRow(r.Context(), "SELECT id FROM messages ORDER BY id DESC LIMIT 1").Scan(&id); err != nil && err != pgx.ErrNoRows {
				tx.Rollback(r.Context())
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, err)
				return
			}

			id = snowflake.MakeSnowflake(id).Increment(time.Now()).Rep()

			if _, err := tx.Exec(r.Context(), "INSERT INTO messages VALUES ($1, $2, $3)", id, username, msg); err != nil {
				tx.Rollback(r.Context())
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, err)
				return
			}

			if _, err := tx.Exec(r.Context(), "NOTIFY messages"); err != nil {
				tx.Rollback(r.Context())
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, err)
				return
			}

			if err := tx.Commit(r.Context()); err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.SerializationFailure {
					continue
				}

				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, err)
				return
			}
			break
		}
	}

	w.WriteHeader(http.StatusNoContent)
}
