package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gabriel-vasile/mimetype"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/util"
	"maunium.net/go/mautrix/util/ffmpeg"

	"go.mau.fi/mautrix-discord/database"
)

func downloadDiscordAttachment(url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for key, value := range discordgo.DroidDownloadHeaders {
		req.Header.Set(key, value)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode > 300 {
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, data)
	}
	return io.ReadAll(resp.Body)
}

func uploadDiscordAttachment(url string, data []byte) error {
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	for key, value := range discordgo.DroidFetchHeaders {
		req.Header.Set(key, value)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode > 300 {
		respData, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, respData)
	}
	return nil
}

func downloadMatrixAttachment(intent *appservice.IntentAPI, content *event.MessageEventContent) ([]byte, error) {
	var file *event.EncryptedFileInfo
	rawMXC := content.URL

	if content.File != nil {
		file = content.File
		rawMXC = file.URL
	}

	mxc, err := rawMXC.Parse()
	if err != nil {
		return nil, err
	}

	data, err := intent.DownloadBytes(mxc)
	if err != nil {
		return nil, err
	}

	if file != nil {
		err = file.DecryptInPlace(data)
		if err != nil {
			return nil, err
		}
	}

	return data, nil
}

func (br *DiscordBridge) uploadMatrixAttachment(intent *appservice.IntentAPI, data []byte, url string, encrypt bool, meta AttachmentMeta) (*database.File, error) {
	dbFile := br.DB.File.New()
	dbFile.Timestamp = time.Now()
	dbFile.URL = url
	dbFile.ID = meta.AttachmentID
	dbFile.EmojiName = meta.EmojiName
	dbFile.Size = len(data)
	dbFile.MimeType = mimetype.Detect(data).String()
	if meta.MimeType == "" {
		meta.MimeType = dbFile.MimeType
	}
	if strings.HasPrefix(meta.MimeType, "image/") {
		cfg, _, _ := image.DecodeConfig(bytes.NewReader(data))
		dbFile.Width = cfg.Width
		dbFile.Height = cfg.Height
	}

	uploadMime := meta.MimeType
	if encrypt {
		dbFile.Encrypted = true
		dbFile.DecryptionInfo = attachment.NewEncryptedFile()
		dbFile.DecryptionInfo.EncryptInPlace(data)
		uploadMime = "application/octet-stream"
	}
	req := mautrix.ReqUploadMedia{
		ContentBytes: data,
		ContentType:  uploadMime,
	}
	if br.Config.Homeserver.AsyncMedia {
		resp, err := intent.UnstableCreateMXC()
		if err != nil {
			return nil, err
		}
		dbFile.MXC = resp.ContentURI
		req.UnstableMXC = resp.ContentURI
		req.UploadURL = resp.UploadURL
		go func() {
			_, err = intent.UploadMedia(req)
			if err != nil {
				br.Log.Errorfln("Failed to upload %s: %v", req.UnstableMXC, err)
				dbFile.Delete()
			}
		}()
	} else {
		uploaded, err := intent.UploadMedia(req)
		if err != nil {
			return nil, err
		}
		dbFile.MXC = uploaded.ContentURI
	}
	return dbFile, nil
}

type AttachmentMeta struct {
	AttachmentID  string
	MimeType      string
	EmojiName     string
	CopyIfMissing bool
	Converter     func([]byte) ([]byte, string, error)
}

var NoMeta = AttachmentMeta{}

type attachmentKey struct {
	URL     string
	Encrypt bool
}

func (br *DiscordBridge) convertLottie(data []byte) ([]byte, string, error) {
	fps := br.Config.Bridge.AnimatedSticker.Args.FPS
	width := br.Config.Bridge.AnimatedSticker.Args.Width
	height := br.Config.Bridge.AnimatedSticker.Args.Height
	target := br.Config.Bridge.AnimatedSticker.Target
	var lottieTarget, outputMime string
	switch target {
	case "png":
		lottieTarget = "png"
		outputMime = "image/png"
		fps = 1
	case "gif":
		lottieTarget = "gif"
		outputMime = "image/gif"
	case "webm":
		lottieTarget = "pngs"
		outputMime = "video/webm"
	case "webp":
		lottieTarget = "pngs"
		outputMime = "image/webp"
	case "disable":
		return data, "application/json", nil
	default:
		return nil, "", fmt.Errorf("invalid animated sticker target %q in bridge config", br.Config.Bridge.AnimatedSticker.Target)
	}

	ctx := context.Background()
	tempdir, err := os.MkdirTemp("", "mautrix_discord_lottie_")
	if err != nil {
		return nil, "", fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer func() {
		removErr := os.RemoveAll(tempdir)
		if removErr != nil {
			br.Log.Warnfln("Failed to delete lottie conversion temp dir: %v", removErr)
		}
	}()

	lottieOutput := filepath.Join(tempdir, "out_")
	if lottieTarget != "pngs" {
		lottieOutput = filepath.Join(tempdir, "output."+lottieTarget)
	}
	cmd := exec.CommandContext(ctx, "lottieconverter", "-", lottieOutput, lottieTarget, fmt.Sprintf("%dx%d", width, height), strconv.Itoa(fps))
	cmd.Stdin = bytes.NewReader(data)
	err = cmd.Run()
	if err != nil {
		return nil, "", fmt.Errorf("failed to run lottieconverter: %w", err)
	}
	var path string
	if lottieTarget == "pngs" {
		var videoCodec string
		outputExtension := "." + target
		if target == "webm" {
			videoCodec = "libvpx-vp9"
		} else if target == "webp" {
			videoCodec = "libwebp_anim"
		} else {
			panic(fmt.Errorf("impossible case: unknown target %q", target))
		}
		path, err = ffmpeg.ConvertPath(
			ctx, lottieOutput+"*.png", outputExtension,
			[]string{"-framerate", strconv.Itoa(fps), "-pattern_type", "glob"},
			[]string{"-c:v", videoCodec, "-pix_fmt", "yuva420p", "-f", target},
			false,
		)
		if err != nil {
			return nil, "", fmt.Errorf("failed to run ffmpeg: %w", err)
		}
	} else {
		path = lottieOutput
	}
	data, err = os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read converted file: %w", err)
	}
	return data, outputMime, nil
}

func (br *DiscordBridge) copyAttachmentToMatrix(intent *appservice.IntentAPI, url string, encrypt bool, meta AttachmentMeta) (returnDBFile *database.File, returnErr error) {
	isCacheable := br.Config.Bridge.CacheMedia != "never" && (br.Config.Bridge.CacheMedia == "always" || !encrypt)
	returnDBFile = br.DB.File.Get(url, encrypt)
	if returnDBFile == nil {
		transferKey := attachmentKey{url, encrypt}
		once, _ := br.attachmentTransfers.GetOrSet(transferKey, &util.ReturnableOnce[*database.File]{})
		returnDBFile, returnErr = once.Do(func() (onceDBFile *database.File, onceErr error) {
			if isCacheable {
				onceDBFile = br.DB.File.Get(url, encrypt)
				if onceDBFile != nil {
					return
				}
			}

			var data []byte
			data, onceErr = downloadDiscordAttachment(url)
			if onceErr != nil {
				return
			}

			if meta.Converter != nil {
				data, meta.MimeType, onceErr = meta.Converter(data)
				if onceErr != nil {
					onceErr = fmt.Errorf("failed to convert attachment: %w", onceErr)
					return
				}
			}

			onceDBFile, onceErr = br.uploadMatrixAttachment(intent, data, url, encrypt, meta)
			if onceErr != nil {
				return
			}
			if isCacheable {
				onceDBFile.Insert(nil)
			}
			br.attachmentTransfers.Delete(transferKey)
			return
		})
	}
	return
}

func (portal *Portal) getEmojiMXCByDiscordID(emojiID, name string, animated bool) id.ContentURI {
	var url, mimeType, ext string
	if animated {
		url = discordgo.EndpointEmojiAnimated(emojiID)
		mimeType = "image/gif"
		ext = "gif"
	} else {
		url = discordgo.EndpointEmoji(emojiID)
		mimeType = "image/png"
		ext = "png"
	}
	mxc := portal.bridge.Config.Bridge.MediaPatterns.Emoji(emojiID, ext)
	if !mxc.IsEmpty() {
		return mxc
	}
	dbFile, err := portal.bridge.copyAttachmentToMatrix(portal.MainIntent(), url, false, AttachmentMeta{
		AttachmentID: emojiID,
		MimeType:     mimeType,
		EmojiName:    name,
	})
	if err != nil {
		portal.log.Warn().Err(err).Str("emoji_id", emojiID).Msg("Failed to copy emoji to Matrix")
		return id.ContentURI{}
	}
	return dbFile.MXC
}
