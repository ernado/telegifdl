package main

import (
	"context"
	"os"
	"path"
	"path/filepath"

	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"go.uber.org/zap"
	"golang.org/x/xerrors"
)

func upload(ctx context.Context, log *zap.Logger, api *tg.Client, inputDir string) error {
	// Upload all gifs from requested dir.
	entries, err := os.ReadDir(inputDir)
	if err != nil {
		return xerrors.Errorf("dir: %w", err)
	}

	var names []string
	for _, e := range entries {
		if path.Ext(e.Name()) != ".mp4" {
			continue
		}
		names = append(names, filepath.Join(inputDir, e.Name()))
	}
	log.Info("Uploading all gifs from directory",
		zap.String("path", inputDir),
		zap.Int("count", len(names)),
	)

	u := uploader.NewUploader(api)
	for _, name := range names {
		f, err := u.FromPath(ctx, name)
		if err != nil {
			return err
		}

		// Sending gif to self.
		sender := message.NewSender(api).Self()
		upd, err := sender.Media(ctx, message.UploadedDocument(f).GIF())
		if err != nil {
			return err
		}
		var (
			sentID    int
			sentMedia tg.MessageMediaClass
		)
		switch upd := upd.(type) {
		case *tg.UpdateShortSentMessage:
			sentID = upd.ID
			sentMedia = upd.Media
		case *tg.Updates:
			for _, u := range upd.Updates {
				switch u := u.(type) {
				case *tg.UpdateNewMessage:
					msg := u.Message.(*tg.Message)
					sentID = msg.ID
					sentMedia = msg.Media
				}
			}
			if sentID == 0 {
				return xerrors.New("unable to find sent message")
			}
		default:
			return xerrors.Errorf("unexpected update type %T", upd)
		}
		doc, ok := sentMedia.(*tg.MessageMediaDocument).Document.AsNotEmpty()
		if !ok {
			return xerrors.New("unexpected document")
		}
		_, saveErr := api.MessagesSaveGif(ctx, &tg.MessagesSaveGifRequest{
			ID:     doc.AsInput(),
			Unsave: false,
		})
		// Cleanup message.
		if _, deleteErr := sender.Revoke().Messages(ctx, sentID); deleteErr != nil {
			return xerrors.Errorf("delete: %w", err)
		}
		if saveErr != nil {
			return xerrors.Errorf("save: %w", saveErr)
		}

		log.Info("Uploaded gif", zap.String("name", name))
	}

	return nil
}
