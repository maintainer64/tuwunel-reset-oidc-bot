package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"tuwunel-reset-oidc-bot/config"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"
)

// ═══════════════════════════════════════════════════════════
// Types
// ═══════════════════════════════════════════════════════════

type Bot struct {
	client      *mautrix.Client
	crypto      *cryptohelper.CryptoHelper
	db          *sql.DB
	adminRoomID id.RoomID
	botUserID   id.UserID
	domain      string
	pending     map[string]*PendingReset
	mu          sync.Mutex
	cfg         *config.Config
	syncReady   atomic.Bool
}

type PendingReset struct {
	Username  string
	UserID    string
	EventID   string
	DMRoomID  id.RoomID
	ExpiresAt time.Time
}

// ═══════════════════════════════════════════════════════════
// Constructor
// ═══════════════════════════════════════════════════════════

func NewBot(cfg *config.Config) (*Bot, error) {
	domain := strings.TrimPrefix(cfg.Homeserver, "https://")
	domain = strings.TrimPrefix(domain, "http://")
	domain = strings.TrimSuffix(domain, "/")

	client, err := mautrix.NewClient(cfg.Homeserver, "", "")
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %w", err)
	}

	return &Bot{
		client:      client,
		adminRoomID: id.RoomID(cfg.AdminRoomID),
		domain:      domain,
		pending:     make(map[string]*PendingReset),
		cfg:         cfg,
	}, nil
}

// ═══════════════════════════════════════════════════════════
// Persistent storage — pending resets in SQLite
// ═══════════════════════════════════════════════════════════

func (b *Bot) initPendingDB() error {
	dbPath := strings.TrimSuffix(b.cfg.CryptoDBPath, ".db") + "_pending.db"

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return fmt.Errorf("failed to open pending DB at %s: %w", dbPath, err)
	}

	if err := db.Ping(); err != nil {
		return fmt.Errorf("failed to ping pending DB: %w", err)
	}

	// WAL mode для надёжности при крашах
	_, _ = db.Exec("PRAGMA journal_mode=WAL")

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS pending_resets (
			username      TEXT PRIMARY KEY,
			user_id       TEXT NOT NULL,
			event_id      TEXT NOT NULL DEFAULT '',
			dm_room_id    TEXT NOT NULL DEFAULT '',
			expires_at    INTEGER NOT NULL
		)
	`)
	if err != nil {
		return fmt.Errorf("failed to create pending_resets table: %w", err)
	}

	b.db = db
	log.Printf("Pending resets DB: %s", dbPath)
	return nil
}

func (b *Bot) loadPendingFromDB() error {
	rows, err := b.db.Query(
		"SELECT username, user_id, event_id, dm_room_id, expires_at FROM pending_resets",
	)
	if err != nil {
		return fmt.Errorf("failed to query pending resets: %w", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var p PendingReset
		var dmRoomStr string
		var expiresUnix int64

		if err := rows.Scan(
			&p.Username, &p.UserID,
			&p.EventID, &dmRoomStr, &expiresUnix,
		); err != nil {
			return fmt.Errorf("failed to scan row: %w", err)
		}

		p.DMRoomID = id.RoomID(dmRoomStr)
		p.ExpiresAt = time.Unix(expiresUnix, 0)
		b.pending[p.Username] = &p
		count++
	}

	if count > 0 {
		log.Printf("Loaded %d pending reset(s) from database", count)
	}
	return rows.Err()
}

func (b *Bot) savePendingToDB(p *PendingReset) error {
	_, err := b.db.Exec(
		`INSERT OR REPLACE INTO pending_resets
		 (username, user_id, event_id, dm_room_id, expires_at)
		 VALUES (?, ?, ?, ?, ?)`,
		p.Username, p.UserID,
		p.EventID, string(p.DMRoomID), p.ExpiresAt.Unix(),
	)
	if err != nil {
		log.Printf("Failed to save pending reset for %s to DB: %v", p.Username, err)
	}
	return err
}

func (b *Bot) deletePendingFromDB(username string) {
	if _, err := b.db.Exec(
		"DELETE FROM pending_resets WHERE username = ?", username,
	); err != nil {
		log.Printf("Failed to delete pending reset for %s from DB: %v", username, err)
	}
}

// ═══════════════════════════════════════════════════════════
// Bot lifecycle
// ═══════════════════════════════════════════════════════════

func (b *Bot) Start(ctx context.Context) error {
	log.Println("Starting bot...")

	// ── Crypto: логин + E2EE атомарно ──
	cryptoHelper, err := cryptohelper.NewCryptoHelper(
		b.client,
		[]byte(b.cfg.PickleKey),
		b.cfg.CryptoDBPath,
	)
	if err != nil {
		return fmt.Errorf("failed to create crypto helper: %w", err)
	}

	cryptoHelper.LoginAs = &mautrix.ReqLogin{
		Type: mautrix.AuthTypePassword,
		Identifier: mautrix.UserIdentifier{
			Type: mautrix.IdentifierTypeUser,
			User: b.cfg.BotUsername,
		},
		Password: b.cfg.BotPassword,
	}

	if err := cryptoHelper.Init(ctx); err != nil {
		return fmt.Errorf("failed to init crypto: %w", err)
	}

	b.crypto = cryptoHelper
	b.botUserID = b.client.UserID
	log.Printf("Logged in as: %s (device: %s)", b.botUserID, b.client.DeviceID)

	// ── Профиль ──
	if b.cfg.DisplayName != "" {
		if err := b.client.SetDisplayName(ctx, b.cfg.DisplayName); err != nil {
			log.Printf("Warning: failed to set display name: %v", err)
		}
	}
	if b.cfg.AvatarURL != "" {
		avatarURI := id.ContentURIString(b.cfg.AvatarURL).ParseOrIgnore()
		if err := b.client.SetAvatarURL(ctx, avatarURI); err != nil {
			log.Printf("Warning: failed to set avatar: %v", err)
		}
	}

	// ── Persistent storage ──
	if err := b.initPendingDB(); err != nil {
		return fmt.Errorf("failed to init pending DB: %w", err)
	}
	if err := b.loadPendingFromDB(); err != nil {
		return fmt.Errorf("failed to load pending resets: %w", err)
	}

	// ── Syncer ──
	syncer := b.client.Syncer.(*mautrix.DefaultSyncer)

	// Пропуск начальной синхронизации (история)
	syncer.OnSync(func(ctx context.Context, resp *mautrix.RespSync, since string) bool {
		if since != "" && !b.syncReady.Load() {
			b.syncReady.Store(true)
			log.Println("Initial sync completed, now processing events")
		}
		return true
	})

	// Auto-join при приглашении (без приветствия)
	syncer.OnEventType(event.StateMember, func(ctx context.Context, evt *event.Event) {
		if evt.GetStateKey() != b.botUserID.String() {
			return
		}
		if evt.Content.AsMember().Membership != event.MembershipInvite {
			return
		}

		log.Printf("Received invite to %s from %s", evt.RoomID, evt.Sender)

		if _, err := b.client.JoinRoomByID(ctx, evt.RoomID); err != nil {
			log.Printf("Failed to join room %s: %v", evt.RoomID, err)
			return
		}
		log.Printf("Joined room %s", evt.RoomID)
	})

	// Обработка текстовых сообщений (CryptoHelper расшифровывает автоматически)
	syncer.OnEventType(event.EventMessage, func(ctx context.Context, evt *event.Event) {
		if !b.syncReady.Load() {
			return
		}
		b.handleMessage(ctx, evt)
	})

	// Лог нерасшифрованных
	syncer.OnEventType(event.EventEncrypted, func(ctx context.Context, evt *event.Event) {
		if !b.syncReady.Load() {
			return
		}
		log.Printf("Could not decrypt message in room %s from %s (event %s)",
			evt.RoomID, evt.Sender, evt.ID)
	})

	// ── Expiration worker ──
	go b.expirationWorker()

	// ── Sync ──
	log.Println("Starting sync...")
	go func() {
		for {
			if err := b.client.Sync(); err != nil {
				log.Printf("Sync error: %v, retrying in 5s...", err)
				time.Sleep(5 * time.Second)
			}
		}
	}()

	log.Printf("Bot started! User ID: %s", b.botUserID)

	// ── Graceful shutdown ──
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down...")
	b.client.StopSync()
	cryptoHelper.Close()
	if b.db != nil {
		b.db.Close()
	}

	return nil
}

// ═══════════════════════════════════════════════════════════
// Message handling
// ═══════════════════════════════════════════════════════════

func (b *Bot) handleMessage(ctx context.Context, evt *event.Event) {
	sender := evt.Sender.String()
	roomID := evt.RoomID

	if sender == b.botUserID.String() {
		return
	}
	if roomID == b.adminRoomID {
		return
	}

	content := evt.Content.AsMessage()
	if content == nil {
		return
	}

	// Только явные текстовые сообщения (не notice, не image, не emote)
	if content.MsgType != event.MsgText {
		return
	}

	message := strings.TrimSpace(strings.ToLower(content.Body))
	b.debug("Message from %s in %s: %s", sender, roomID, message)

	if message == "!ping" {
		b.sendReply(roomID, evt, "🏓 pong!")
		return
	}

	if message == "сброс" || message == "reset" ||
		message == "сбросить" || message == "сброс пароля" {
		username := b.extractUsername(sender)
		if username != "" {
			b.handleResetRequest(roomID, evt, sender, username)
		}
		return
	}

	b.sendReply(roomID, evt,
		"🔐 **Сброс пароля**\n\n"+
			"Для сброса пароля напишите `сброс` или `reset`")
}

// ═══════════════════════════════════════════════════════════
// Sending messages (Markdown + Reply)
// ═══════════════════════════════════════════════════════════

func (b *Bot) sendReply(roomID id.RoomID, replyTo *event.Event, markdown string) (*mautrix.RespSendEvent, error) {
	content := format.RenderMarkdown(markdown, true, true)
	content.SetReply(replyTo)
	resp, err := b.client.SendMessageEvent(context.Background(), roomID, event.EventMessage, &content)
	if err != nil {
		log.Printf("Failed to send reply to %s: %v", roomID, err)
		return nil, err
	}
	return resp, nil
}

func (b *Bot) sendMarkdown(roomID id.RoomID, markdown string) (*mautrix.RespSendEvent, error) {
	content := format.RenderMarkdown(markdown, true, true)
	resp, err := b.client.SendMessageEvent(context.Background(), roomID, event.EventMessage, &content)
	if err != nil {
		log.Printf("Failed to send message to %s: %v", roomID, err)
		return nil, err
	}
	return resp, nil
}

// ═══════════════════════════════════════════════════════════
// Password reset logic
// ═══════════════════════════════════════════════════════════

func (b *Bot) handleResetRequest(roomID id.RoomID, evt *event.Event, sender, username string) {
	b.mu.Lock()
	if existing, exists := b.pending[username]; exists {
		b.mu.Unlock()
		remaining := time.Until(existing.ExpiresAt).Round(time.Second)
		b.sendReply(roomID, evt, fmt.Sprintf(
			"⏳ У вас уже есть активный запрос. Осталось **%s**.", remaining))
		return
	}
	b.mu.Unlock()

	tempPassword := b.generatePassword()

	// Команда в админ-комнату
	adminCmd := fmt.Sprintf("!admin users reset-password %s %s", username, tempPassword)
	if _, err := b.sendMarkdown(b.adminRoomID, adminCmd); err != nil {
		b.sendReply(roomID, evt, fmt.Sprintf("❌ Ошибка отправки команды: %v", err))
		log.Printf("Failed to send admin command: %v", err)
		return
	}

	// Временный пароль пользователю (reply)
	loginMsg := fmt.Sprintf(
		"🔑 **Временный пароль**\n\n"+
			"Логин: `%s`\nПароль: `%s`\n\n"+
			"⚠️ Пароль действителен **5 минут**!",
		username, tempPassword)

	resp, err := b.sendReply(roomID, evt, loginMsg)
	if err != nil {
		b.sendReply(roomID, evt, "❌ Ошибка отправки пароля")
		return
	}

	expiresAt := time.Now().Add(5 * time.Minute)

	pending := &PendingReset{
		Username:  username,
		UserID:    sender,
		EventID:   resp.EventID.String(),
		DMRoomID:  roomID,
		ExpiresAt: expiresAt,
	}

	// Сохраняем в память
	b.mu.Lock()
	b.pending[username] = pending
	b.mu.Unlock()

	// Сохраняем в БД (переживёт перезапуск)
	if err := b.savePendingToDB(pending); err != nil {
		log.Printf("Warning: failed to persist pending reset for %s: %v", username, err)
	}

	log.Printf("Issued temporary password for %s, expires at %s", username, expiresAt)
}

// ═══════════════════════════════════════════════════════════
// Expiration worker — каждые 60 секунд
// ═══════════════════════════════════════════════════════════

func (b *Bot) expirationWorker() {
	// Ждём завершения initial sync, чтобы бот знал
	// состояние комнат (E2EE) и мог шифровать сообщения
	for !b.syncReady.Load() {
		time.Sleep(time.Second)
	}

	// Сразу после sync обрабатываем то, что истекло пока бот был выключен
	b.processExpiredResets()

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		b.processExpiredResets()
	}
}

func (b *Bot) processExpiredResets() {
	// Собираем истёкшие, сразу удаляем из памяти
	b.mu.Lock()
	now := time.Now()
	var expired []*PendingReset
	for username, pending := range b.pending {
		if now.After(pending.ExpiresAt) {
			expired = append(expired, pending)
			delete(b.pending, username)
		}
	}
	b.mu.Unlock()

	// Обрабатываем без блокировки (сетевые операции)
	for _, pending := range expired {
		log.Printf("Expiring temporary password for: %s", pending.Username)

		// Сбрасываем пароль на случайный (пользователь не сможет войти по старому)
		newPassword := b.generatePassword()
		adminCmd := fmt.Sprintf("!admin users reset-password %s %s", pending.Username, newPassword)
		if _, err := b.sendMarkdown(b.adminRoomID, adminCmd); err != nil {
			log.Printf("Failed to expire password for %s: %v", pending.Username, err)
		}

		// Удаляем сообщение с паролем
		if pending.EventID != "" && pending.DMRoomID != "" {
			if _, err := b.client.RedactEvent(context.Background(),
				pending.DMRoomID, id.EventID(pending.EventID)); err != nil {
				log.Printf("Failed to redact password message for %s: %v", pending.Username, err)
			}
		}

		// Уведомляем пользователя
		if pending.DMRoomID != "" {
			b.sendMarkdown(pending.DMRoomID,
				"⏰ Время действия временного пароля **истекло**.\n\n"+
					"Запросите новый командой `сброс`.")
		}

		// Удаляем из БД ПОСЛЕДНИМ — если бот крашнется до этого,
		// при рестарте запись загрузится и обработается повторно
		b.deletePendingFromDB(pending.Username)

		log.Printf("Expired and cleaned up for: %s", pending.Username)
	}
}

// ═══════════════════════════════════════════════════════════
// Utilities
// ═══════════════════════════════════════════════════════════

func (b *Bot) extractUsername(userID string) string {
	userID = strings.TrimPrefix(userID, "@")
	parts := strings.Split(userID, ":")
	if len(parts) > 0 {
		return parts[0]
	}
	return userID
}

func (b *Bot) generatePassword() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

func (b *Bot) debug(fmt_ string, args ...interface{}) {
	if b.cfg.Debug {
		log.Printf("[DEBUG] "+fmt_, args...)
	}
}
