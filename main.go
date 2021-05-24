package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gotd/contrib/middleware/ratelimit"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/crypto/ssh/terminal"
	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"
	"golang.org/x/xerrors"
)

// terminalAuth implements auth.UserAuthenticator prompting the terminal for
// input.
type terminalAuth struct{}

func (terminalAuth) SignUp(ctx context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, xerrors.New("not implemented")
}

func (terminalAuth) AcceptTermsOfService(ctx context.Context, tos tg.HelpTermsOfService) error {
	return &auth.SignUpRequired{TermsOfService: tos}
}

func (terminalAuth) Code(ctx context.Context, sentCode *tg.AuthSentCode) (string, error) {
	fmt.Print("Enter code: ")
	code, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(code), nil
}

func (terminalAuth) Phone(_ context.Context) (string, error) {
	fmt.Print("Enter phone: ")
	code, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(code), nil
}

func (terminalAuth) Password(_ context.Context) (string, error) {
	fmt.Print("Enter 2FA password: ")
	bytePwd, err := terminal.ReadPassword(syscall.Stdin)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(bytePwd)), nil
}

func run(ctx context.Context) error {
	var (
		outputDir = flag.String("out", os.TempDir(), "output directory")
		jobs      = flag.Int("j", 3, "maximum concurrent download jobs")
		rateLimit = flag.Duration("rate", time.Millisecond*100, "limit maximum rpc call rate")
		rateBurst = flag.Int("rate-burst", 3, "limit rpc call burst")
	)
	flag.Parse()

	log, _ := zap.NewDevelopment(zap.IncreaseLevel(zapcore.InfoLevel), zap.AddStacktrace(zapcore.FatalLevel))
	defer func() { _ = log.Sync() }()

	// Initializing client from environment.
	// Available environment variables:
	// 	APP_ID:         app_id of Telegram app.
	// 	APP_HASH:       app_hash of Telegram app.
	// 	SESSION_FILE:   path to session file
	// 	SESSION_DIR:    path to session directory, if SESSION_FILE is not set
	client, err := telegram.ClientFromEnvironment(telegram.Options{
		Logger: log,
	})
	if err != nil {
		return err
	}

	// Setting up authentication flow.
	// Current flow will read phone, code and 2FA password from terminal.
	flow := auth.NewFlow(terminalAuth{}, auth.SendCodeOptions{})

	// Creating new RPC client.
	//
	// The tg.Client is generated from Telegram schema and implements
	// invocation of all defined Telegram MTProto methods on top of tg.Invoker.
	// E.g. api.MessagesSendMessage() is messages.sendMessage method.
	//
	// The tg.Invoker interface is implemented by client (telegram.Client) and
	// allows calling any MTProto method, like that:
	//	InvokeRaw(ctx context.Context, input bin.Encoder, output bin.Decoder) error
	api := tg.NewClient(
		// Wrapping invoker and rate-limiting RPC calls.
		ratelimit.Middleware(rate.NewLimiter(rate.Every(*rateLimit), *rateBurst))(client),
	)

	// Connecting, performing authentication and downloading gifs.
	return client.Run(ctx, func(ctx context.Context) error {
		// Perform auth if no session is available.
		if err := client.Auth().IfNecessary(ctx, flow); err != nil {
			return xerrors.Errorf("auth: %w", err)
		}

		// Processing gifs.
		gifs := make(chan *tg.Document, *jobs)
		g, ctx := errgroup.WithContext(ctx)
		g.Go(func() error {
			defer close(gifs)

			result, err := api.MessagesGetSavedGifs(ctx, 0)
			if err != nil {
				return xerrors.Errorf("get: %w", err)
			}

			switch result := result.(type) {
			case *tg.MessagesSavedGifsNotModified:
				// Should not be reachable, means that result by paginationHash was not changed.
				return nil
			case *tg.MessagesSavedGifs:
				if len(result.Gifs) == 0 {
					// No more results.
					return nil
				}

				// Processing batch.
				for _, doc := range result.Gifs {
					doc, ok := doc.AsNotEmpty()
					if !ok {
						continue
					}

					gifs <- doc
				}
			}

			return nil
		})

		for j := 0; j < *jobs; j++ {
			g.Go(func() error {
				// Process all discovered gifs.
				d := downloader.NewDownloader()
				for doc := range gifs {
					gifPath := filepath.Join(*outputDir, fmt.Sprintf("%d.mp4", doc.ID))
					log.Info("Got GIF",
						zap.Int64("id", doc.ID),
						zap.Time("date", time.Unix(int64(doc.Date), 0)),
						zap.String("path", gifPath),
					)

					if _, err := os.Stat(gifPath); err == nil {
						// File exists, skipping.
						continue
					}

					// Downloading gif to gifPath.
					loc := doc.AsInputDocumentFileLocation()
					if _, err := d.Download(api, loc).ToPath(ctx, gifPath); err != nil {
						return xerrors.Errorf("download: %w", err)
					}
				}

				return nil
			})
		}

		return g.Wait()
	})
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx); err != nil {
		panic(err)
	}
}
