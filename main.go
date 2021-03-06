package main

import (
	"bufio"
	"flag"
	"fmt"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/jcxplorer/cwlogger"
	"gopkg.in/tylerb/graceful.v1"
	"io"
	"log"
	"net/http"
	"octoberswimmer/heroku-cloudwatch-drain/logparser"
	"os"
	"regexp"
	"sync"
	"time"
)

// App is a Heroku HTTPS log drain. It receives log batches as POST requests,
// parses them, and sends them to CloudWatch Logs.
type App struct {
	retention      int
	stripAnsiCodes bool
	user, pass     string
	parse          logparser.ParseFunc

	loggers map[string]logger
	mu      sync.Mutex // protects loggers
}

type logger interface {
	Log(t time.Time, s string)
	Close()
}

func main() {
	var bind, user, pass string
	var retention int
	var stripAnsiCodes bool
	var err error

	flag.StringVar(&bind, "bind", ":8080", "address to bind to")
	flag.IntVar(&retention, "retention", 0, "log retention in days for new log groups")
	flag.StringVar(&user, "user", "", "username for HTTP basic auth")
	flag.StringVar(&pass, "pass", "", "password for HTTP basic auth")
	flag.BoolVar(&stripAnsiCodes, "strip-ansi-codes", false, "strip ANSI codes from log messages")
	flag.Parse()

	app := &App{
		retention:      retention,
		user:           user,
		pass:           pass,
		stripAnsiCodes: stripAnsiCodes,
		parse:          logparser.Parse,
		loggers:        make(map[string]logger),
	}

	mux := http.NewServeMux()
	mux.Handle("/", app)
	err = graceful.RunWithErr(bind, 5*time.Second, mux)
	if err != nil {
		log.Println(err)
		os.Exit(1)
	}

	app.Stop()
}

func (app *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	appName := r.URL.Path[1:]

	if r.Method == http.MethodGet {
		if appName == "" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
		} else {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("Not found"))
		}
		return
	}

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("The only accepted request method is POST"))
		return
	}

	if appName == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Request path must specify the log group name"))
		return
	}

	user, pass, _ := r.BasicAuth()
	if user != app.user || pass != app.pass {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	l, err := app.logger(appName)
	if err != nil {
		log.Printf("failed to create logger for app %s: %s\n", appName, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = app.processMessages(r.Body, l); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Println(err)
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

// Stop all the loggers, flushing any pending requests.
func (app *App) Stop() {
	var wg sync.WaitGroup
	wg.Add(len(app.loggers))
	app.mu.Lock()
	defer app.mu.Unlock()
	for _, l := range app.loggers {
		go func(l logger) {
			l.Close()
			wg.Done()
		}(l)
	}
	wg.Wait()
}

func (app *App) logger(appName string) (l logger, err error) {
	app.mu.Lock()
	defer app.mu.Unlock()
	l, ok := app.loggers[appName]
	if !ok {
		l, err = cwlogger.New(&cwlogger.Config{
			LogGroupName: appName,
			Retention:    app.retention,
			Client:       cloudwatchlogs.New(session.New()),
		})
		app.loggers[appName] = l
	}
	return l, err
}

func (app *App) processMessages(r io.Reader, l logger) error {
	buf := bufio.NewReader(r)
	eof := false
	for {
		b, err := buf.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				eof = true
			} else {
				return fmt.Errorf("failed to scan request body: %s", err)
			}
		}
		if eof && len(b) == 0 {
			break
		}
		entry, err := app.parse(b)
		if err != nil {
			return fmt.Errorf("unable to parse message: %s, error: %s", string(b), err)
		}
		m := entry.Message
		if app.stripAnsiCodes {
			m = stripAnsi(m)
		}
		if !eof {
			m = m[:len(m)-1]
		}
		l.Log(entry.Time, m)
		if eof {
			break
		}
	}
	return nil
}

var ansiRegexp = regexp.MustCompile("\x1b[^m]*m")

func stripAnsi(s string) string {
	return ansiRegexp.ReplaceAllLiteralString(s, "")
}
