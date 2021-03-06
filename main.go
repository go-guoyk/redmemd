package main

import (
	"bufio"
	"context"
	"github.com/bsm/redislock"
	"github.com/go-redis/redis/v8"
	"go.guoyk.net/redmemd/memwire"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	optPort     = strings.TrimSpace(os.Getenv("PORT"))
	optRedisURL = strings.TrimSpace(os.Getenv("REDIS_URL"))
	optDebug, _ = strconv.ParseBool(os.Getenv("DEBUG"))
)

func main() {
	var err error
	defer func(err *error) {
		if *err != nil {
			log.Println("exited with error:", (*err).Error())
			os.Exit(1)
		} else {
			log.Println("exited")
		}
	}(&err)

	rand.Seed(time.Now().UnixNano())

	if optPort == "" {
		optPort = "11211"
	}

	if optRedisURL == "" {
		optRedisURL = "redis://127.0.0.1:6379/0"
	}

	var addr *net.TCPAddr
	if addr, err = net.ResolveTCPAddr("tcp", "0.0.0.0:"+optPort); err != nil {
		return
	}

	log.Println("using addr:", addr.String())

	var listener *net.TCPListener
	if listener, err = net.ListenTCP("tcp", addr); err != nil {
		return
	}

	var redisOptions *redis.Options
	if redisOptions, err = redis.ParseURL(optRedisURL); err != nil {
		return
	}

	log.Println("using redis:", redisOptions.Addr)

	ctx, ctxCancel := context.WithCancel(context.Background())

	wg := &sync.WaitGroup{}

	chErr := make(chan error, 1)
	chSig := make(chan os.Signal, 1)

	signal.Notify(chSig, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		for {
			if conn, err1 := listener.AcceptTCP(); err1 != nil {
				chErr <- err1
				return
			} else {
				wg.Add(1)
				go handleConn(ctx, wg, conn)
			}
		}
	}()

	select {
	case err = <-chErr:
	case sig := <-chSig:
		log.Println("signal caught:", sig.String())
	}

	_ = listener.Close()

	ctxCancel()

	log.Println("waiting for existed connections")
	wg.Wait()
}

func handleConn(ctx context.Context, wg *sync.WaitGroup, conn *net.TCPConn) {
	defer wg.Done()
	defer conn.Close()

	log.Println("connected:", conn.RemoteAddr().String())
	defer log.Println("disconnected:", conn.RemoteAddr().String())

	var err error
	defer func(err *error) {
		if *err == io.EOF {
			*err = nil
		}
		if *err != nil {
			log.Println("error:", conn.RemoteAddr().String(), (*err).Error())
		}
	}(&err)

	var opts *redis.Options

	if opts, err = redis.ParseURL(optRedisURL); err != nil {
		return
	}

	client := redis.NewClient(opts)
	defer client.Close()

	rlock := redislock.New(client)

	if err = client.Ping(ctx).Err(); err != nil {
		return
	}

	r := bufio.NewReaderSize(conn, 4096)
	w := bufio.NewWriterSize(conn, 4096)

	go func() {
		<-ctx.Done()
		time.Sleep(time.Second)
		_ = conn.Close()
	}()

	for {
		var req *memwire.Request
		if req, err = memwire.ReadRequest(r); err != nil {
			if err == io.EOF {
				return
			}
			if optDebug {
				log.Println("[debug] read error:", conn.RemoteAddr().String(), err.Error())
			}
			if _, ok := err.(memwire.Error); ok {
				if _, err = w.WriteString(memwire.CodeErr + "\r\n"); err != nil {
					return
				}
				if err = w.Flush(); err != nil {
					return
				}
				continue
			} else {
				return
			}
		}

		if ctx.Err() != nil {
			if _, err = w.WriteString(memwire.CodeServerErr + " shutting down\r\n"); err != nil {
				return
			}
			if err = w.Flush(); err != nil {
				return
			}
			return
		}

		rt := &RoundTripper{
			Request:        req,
			Debug:          optDebug,
			Redis:          client,
			RedisLock:      rlock,
			ResponseWriter: w,
		}
		if err = rt.Do(ctx); err != nil {
			return
		}
	}
}
