package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"golang.org/x/sys/unix"

	"sohio.net/go4fun/internal/broadcast"
	"sohio.net/go4fun/internal/chat"
	"sohio.net/go4fun/internal/session"
	"sohio.net/go4fun/internal/static"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, unix.SIGTERM, unix.SIGHUP)
	defer cancel()

	var cs broadcast.Set[string]

	pool, err := pgxpool.New(ctx, "postgres://postgres@localhost:5432/postgres?sslmode=disable")
	if err != nil {
		log.Fatalln("pool init:", err)
	}

	// Listen for notifications and forward to broadcast set
	go func() {
		// If we can't listen, what's the point?
		defer cancel()

		pc, err := pool.Acquire(ctx)
		if err != nil {
			if err != context.Canceled {
				fmt.Fprintln(os.Stderr, "notification listener init:", err)
			}
			return
		}

		c := pc.Hijack()
		defer c.Close(ctx)

		c.Exec(ctx, "LISTEN messages")

		for {
			n, err := c.WaitForNotification(ctx)
			if err != nil {
				if err != context.Canceled {
					fmt.Fprintln(os.Stderr, "notification listener:", err)
				}
				return
			}
			cs.Send(n.Payload)
		}
	}()

	mux := http.NewServeMux()

	mux.Handle("/static/", static.NewHandler())

	chat, err := chat.NewHandler(pool, &cs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "chat init:", err)
		return
	}
	mux.Handle("/chat/", chat)

	session, err := session.NewHandler(mux, pool)
	if err != nil {
		fmt.Fprintln(os.Stderr, "session init:", err)
		return
	}

	srv := http.Server{
		BaseContext: func(net.Listener) context.Context { return ctx },
		Handler:     h2c.NewHandler(session, &http2.Server{}),
	}

	var l net.Listener

	if true {
		var err error
		if l, err = net.Listen("tcp", ":9000"); err != nil {
			fmt.Fprintln(os.Stderr, "server init:", err)
			return
		}
	}

	// os.Remove("./hello.sock")
	// listener, err := net.Listen("unix", "./hello.sock")

	// if err != nil {
	// 	fmt.Println("Error:", err)
	// 	return
	// }

	shutdownFinished := make(chan struct{})
	context.AfterFunc(ctx, func() {
		if err := srv.Shutdown(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, "server shutdown:", err)
		}
		close(shutdownFinished)
	})

	if err := srv.Serve(l); err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "server:", err)
	}

	<-shutdownFinished
}
