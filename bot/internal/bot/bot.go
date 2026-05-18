package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"llm-telegram-bot/internal/config"
	"llm-telegram-bot/internal/llm"
	"llm-telegram-bot/internal/messages"
	"llm-telegram-bot/internal/store"
	"llm-telegram-bot/internal/textutil"
	"llm-telegram-bot/internal/tools"
)

const (
	// Implementation safety bounds — not exposed as env vars. Changing these
	// can break tool calling, streaming UX, or Telegram protocol limits.
	maxToolRounds    = 4    // cap runaway tool-call loops
	editMaxAttempts  = 3    // streaming preview edits; failures fall back to a new message
	editMaxRetryWait = 3 * time.Second
	maxMsgLen        = 4000 // Telegram per-message ceiling is 4096 chars
	// On exceed_context_size_error, the live history is wiped and the user's
	// message re-fed up to this many times. After that we give up, discard the
	// message, and post contextOverflowGiveUpMsg.
	maxContextOverflowRetries = 2

	// Fixed string sentinel that prefixes auto-summary blobs in stored history.
	// Kept in code (not in messages.json) because save/load logic and trim
	// logic both pattern-match it — changing the marker requires a migration.
	autoSummaryMarker = "[CONTEXTO ANTERIOR]"
)

type Bot struct {
	cfg      *config.Config
	tg       *telego.Bot
	store    *store.Store
	llm      *llm.Client
	tools    *tools.Registry
	msgs     *messages.Loader
	username string

	// rootCtx is set by Run; spawned goroutines use it so SIGTERM propagates.
	rootCtx context.Context

	inflightMu sync.Mutex
	inflight   map[int64]bool

	// Serializes load→modify→save on the per-chat history blob.
	chatLocks sync.Map // map[int64]*sync.Mutex

	spontMu   sync.Mutex
	spontData map[int64]*spontaneousState

	summarizingMu sync.Mutex
	summarizing   map[int64]bool
	lastSummaryAt map[int64]time.Time
	summaryCancel map[int64]context.CancelFunc

	// Per-chat rate-limit on the "context getting full" warning we post in
	// chat — we don't want to spam every turn once the threshold is crossed.
	contextWarnMu sync.Mutex
	contextWarnAt map[int64]time.Time

	// mtime-keyed caches for files re-read on every turn / every message.
	promptMu     sync.RWMutex
	promptMtime  time.Time
	promptCached string
	namesMu      sync.RWMutex
	namesMtime   time.Time
	namesCached  map[string]string
}

type spontaneousState struct {
	count     int
	threshold int
}

func New(cfg *config.Config, db *store.Store, llmClient *llm.Client, registry *tools.Registry, msgs *messages.Loader) (*Bot, error) {
	tg, err := telego.NewBot(cfg.TelegramToken)
	if err != nil {
		return nil, err
	}
	me, err := tg.GetMe(context.Background())
	if err != nil {
		return nil, fmt.Errorf("get me: %w", err)
	}
	return &Bot{
		cfg:           cfg,
		tg:            tg,
		store:         db,
		llm:           llmClient,
		tools:         registry,
		msgs:          msgs,
		username:      me.Username,
		inflight:      map[int64]bool{},
		spontData:     map[int64]*spontaneousState{},
		summarizing:   map[int64]bool{},
		lastSummaryAt: map[int64]time.Time{},
		summaryCancel: map[int64]context.CancelFunc{},
		contextWarnAt: map[int64]time.Time{},
	}, nil
}

func (b *Bot) TG() *telego.Bot { return b.tg }

func (b *Bot) claimInflight(chatID int64) bool {
	b.inflightMu.Lock()
	defer b.inflightMu.Unlock()
	if b.inflight[chatID] {
		return false
	}
	b.inflight[chatID] = true
	return true
}

func (b *Bot) releaseInflight(chatID int64) {
	b.inflightMu.Lock()
	defer b.inflightMu.Unlock()
	delete(b.inflight, chatID)
}

func (b *Bot) chatLock(chatID int64) *sync.Mutex {
	actual, _ := b.chatLocks.LoadOrStore(chatID, &sync.Mutex{})
	return actual.(*sync.Mutex)
}

func (b *Bot) appendMessage(chatID int64, m store.Message) {
	lock := b.chatLock(chatID)
	lock.Lock()
	defer lock.Unlock()

	msgs, err := b.store.Load(chatID)
	if err != nil {
		log.Printf("append load: %v", err)
		return
	}
	msgs = append(msgs, m)
	if cap := b.cfg.MaxStoredMessages; cap > 0 && len(msgs) > cap {
		msgs = msgs[len(msgs)-cap:]
	}
	if err := b.store.Save(chatID, msgs); err != nil {
		log.Printf("append save: %v", err)
	}
	// Long-term FTS index — survives history trim, used by the `recall` tool.
	if err := b.store.IndexMessage(chatID, m.Role, m.Content, time.Now()); err != nil {
		log.Printf("append index: %v", err)
	}
}

func (b *Bot) tickSpontaneous(chatID int64) bool {
	b.spontMu.Lock()
	defer b.spontMu.Unlock()
	st, ok := b.spontData[chatID]
	if !ok {
		st = &spontaneousState{threshold: b.randomThreshold()}
		b.spontData[chatID] = st
	}
	st.count++
	log.Printf("spontaneous tick: chat=%d count=%d threshold=%d", chatID, st.count, st.threshold)
	if st.count >= st.threshold {
		st.count = 0
		st.threshold = b.randomThreshold()
		return true
	}
	return false
}

func (b *Bot) randomThreshold() int {
	span := b.cfg.SpontaneousMax - b.cfg.SpontaneousMin + 1
	if span < 1 {
		span = 1
	}
	return b.cfg.SpontaneousMin + rand.Intn(span)
}

func buildLLMMessages(systemPrompt string, history []store.Message) []llm.Message {
	out := make([]llm.Message, 0, len(history)+1)
	out = append(out, llm.Message{Role: "system", Content: systemPrompt})
	for _, m := range history {
		out = append(out, llm.Message{Role: m.Role, Content: m.Content})
	}
	return out
}

// recentAssistantBlock returns a system-role reminder of the bot's last N
// assistant turns so it can avoid echoing them. Empty string if none.
func recentAssistantBlock(header string, history []store.Message, n int) string {
	if n <= 0 {
		return ""
	}
	picks := make([]string, 0, n)
	for i := len(history) - 1; i >= 0 && len(picks) < n; i-- {
		if history[i].Role != "assistant" {
			continue
		}
		c := strings.TrimSpace(history[i].Content)
		if c == "" {
			continue
		}
		if len(c) > 240 {
			c = c[:240] + "…"
		}
		picks = append(picks, "- "+c)
	}
	if len(picks) == 0 {
		return ""
	}
	// Reverse to chronological (oldest → newest) for natural reading.
	for i, j := 0, len(picks)-1; i < j; i, j = i+1, j-1 {
		picks[i], picks[j] = picks[j], picks[i]
	}
	return header + "\n" + strings.Join(picks, "\n")
}

func (b *Bot) spontaneousReply(ctx context.Context, chatID int64) {
	if !b.claimInflight(chatID) {
		return
	}
	defer b.releaseInflight(chatID)

	history, err := b.store.Load(chatID)
	if err != nil {
		return
	}
	history = trimHistoryByTokens(history, b.cfg.HistoryTokenBudget)
	if len(history) == 0 {
		return
	}

	m := b.msgs.Get()
	llmMsgs := buildLLMMessages(b.systemPromptForChat(chatID), history)
	if recent := recentAssistantBlock(m.UI.RecentAssistantHeader, history, 3); recent != "" {
		llmMsgs = append(llmMsgs, llm.Message{Role: "system", Content: recent})
	}
	llmMsgs = append(llmMsgs, llm.Message{
		Role:    "user",
		Content: m.Prompts.Spontaneous,
	})

	log.Printf("spontaneous: chat=%d firing", chatID)
	final, _, err := b.llm.Chat(ctx, llmMsgs, nil, nil)
	if err != nil {
		log.Printf("spontaneous: %v", err)
		return
	}
	final = strings.TrimSpace(final)
	if final == "" || strings.EqualFold(final, "SKIP") {
		log.Printf("spontaneous: chat=%d model chose to skip", chatID)
		return
	}
	final = textutil.Ellipsize(final, maxMsgLen)
	if _, err := b.tg.SendMessage(ctx, tu.Message(tu.ID(chatID), final)); err != nil {
		log.Printf("spontaneous send: %v", err)
		return
	}
	log.Printf("spontaneous reply: chat=%d text=%q", chatID, textutil.Ellipsize(final, 200))
	b.appendMessage(chatID, store.Message{Role: "assistant", Content: final})
}

func (b *Bot) tryReact(ctx context.Context, msg *telego.Message) {
	if msg == nil || msg.Text == "" {
		return
	}
	for _, p := range b.msgs.Get().Reactions {
		if p.RE == nil || !p.RE.MatchString(msg.Text) {
			continue
		}
		if rand.Float64() > b.cfg.ReactionProbability {
			return
		}
		err := b.tg.SetMessageReaction(ctx, &telego.SetMessageReactionParams{
			ChatID:    tu.ID(msg.Chat.ID),
			MessageID: msg.MessageID,
			Reaction: []telego.ReactionType{
				&telego.ReactionTypeEmoji{Type: "emoji", Emoji: p.Emoji},
			},
		})
		if err != nil {
			log.Printf("react: emoji=%s msg=%d text=%q err=%v",
				p.Emoji, msg.MessageID, textutil.Ellipsize(msg.Text, 60), err)
			return
		}
		log.Printf("react: chat=%d msg=%d emoji=%s", msg.Chat.ID, msg.MessageID, p.Emoji)
		return
	}
}

func (b *Bot) onDemandSummary(ctx context.Context, chatID int64) {
	m := b.msgs.Get()
	if !b.claimInflight(chatID) {
		_, _ = b.tg.SendMessage(ctx, tu.Message(tu.ID(chatID), m.UI.BusyOther))
		return
	}
	defer b.releaseInflight(chatID)

	history, err := b.store.Load(chatID)
	if err != nil {
		log.Printf("summary load: %v", err)
		return
	}
	if len(history) < 3 {
		_, _ = b.tg.SendMessage(ctx, tu.Message(tu.ID(chatID), m.UI.NothingToSummarize))
		return
	}
	history = trimHistoryByTokens(history, b.cfg.HistoryTokenBudget)

	llmMsgs := buildLLMMessages(b.systemPromptForChat(chatID), history)
	llmMsgs = append(llmMsgs, llm.Message{
		Role:    "user",
		Content: m.Prompts.SummaryOnDemand,
	})

	log.Printf("summary: chat=%d generating", chatID)
	final, _, err := b.llm.Chat(ctx, llmMsgs, nil, nil)
	if err != nil {
		log.Printf("summary: %v", err)
		_, _ = b.tg.SendMessage(ctx, tu.Message(tu.ID(chatID), m.UI.GenericError))
		return
	}
	final = strings.TrimSpace(final)
	if final == "" {
		final = m.UI.EmptySummary
	}
	final = textutil.Ellipsize(final, maxMsgLen)
	_, _ = b.tg.SendMessage(ctx, tu.Message(tu.ID(chatID), final))
	log.Printf("summary reply: chat=%d text=%q", chatID, textutil.Ellipsize(final, 200))
}

func (b *Bot) maybeAutoSummarize(chatID int64) {
	n, err := b.store.Count(chatID)
	if err != nil || n < b.cfg.AutoSummaryAtMessages {
		return
	}
	b.summarizingMu.Lock()
	if b.summarizing[chatID] {
		b.summarizingMu.Unlock()
		return
	}
	if last, ok := b.lastSummaryAt[chatID]; ok && time.Since(last) < b.cfg.AutoSummaryCooldown {
		b.summarizingMu.Unlock()
		return
	}
	b.summarizing[chatID] = true
	b.lastSummaryAt[chatID] = time.Now()
	b.summarizingMu.Unlock()
	go b.autoSummarize(chatID)
}

func (b *Bot) cancelAutoSummary(chatID int64) {
	b.summarizingMu.Lock()
	cancel := b.summaryCancel[chatID]
	b.summarizingMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// autoSummarize compresses the oldest (count - keepRecent) messages into a
// single "[CONTEXTO ANTERIOR]"-prefixed user message at history[0]. Uses a
// cancellable context so an addressed user message can interrupt it.
func (b *Bot) autoSummarize(chatID int64) {
	ctx, cancel := context.WithCancel(b.rootCtx)
	b.summarizingMu.Lock()
	b.summaryCancel[chatID] = cancel
	b.summarizingMu.Unlock()
	defer func() {
		b.summarizingMu.Lock()
		delete(b.summarizing, chatID)
		delete(b.summaryCancel, chatID)
		b.summarizingMu.Unlock()
		cancel()
	}()

	// Politely wait for an idle moment, but bail out if cancelled.
	var gotLock bool
	for tries := 0; tries < 10; tries++ {
		if b.claimInflight(chatID) {
			gotLock = true
			break
		}
		select {
		case <-ctx.Done():
			log.Printf("auto-summary: chat=%d cancelled while waiting", chatID)
			return
		case <-time.After(30 * time.Second):
		}
	}
	if !gotLock {
		log.Printf("auto-summary: chat=%d gave up waiting for inflight", chatID)
		return
	}
	defer b.releaseInflight(chatID)

	lock := b.chatLock(chatID)
	lock.Lock()
	msgs, err := b.store.Load(chatID)
	lock.Unlock()
	if err != nil || len(msgs) < b.cfg.AutoSummaryAtMessages {
		return
	}

	// Separate existing summary (if any) and recent tail; the middle gets compressed.
	var existingSummary *store.Message
	if strings.HasPrefix(msgs[0].Content, autoSummaryMarker) {
		s := msgs[0]
		existingSummary = &s
		msgs = msgs[1:]
	}
	keepRecent := b.cfg.AutoSummaryKeepRecent
	if len(msgs) <= keepRecent {
		return
	}
	toCompress := msgs[:len(msgs)-keepRecent]
	keep := msgs[len(msgs)-keepRecent:]

	llmMsgs := []llm.Message{{Role: "system", Content: b.systemPromptForChat(chatID)}}
	if existingSummary != nil {
		llmMsgs = append(llmMsgs, llm.Message{Role: "user", Content: existingSummary.Content})
	}
	for _, m := range toCompress {
		llmMsgs = append(llmMsgs, llm.Message{Role: m.Role, Content: m.Content})
	}
	llmMsgs = append(llmMsgs, llm.Message{
		Role:    "user",
		Content: b.msgs.Get().Prompts.SummaryAuto,
	})

	log.Printf("auto-summary: chat=%d compressing %d msgs", chatID, len(toCompress))
	summary, _, err := b.llm.Chat(ctx, llmMsgs, nil, nil)
	if err != nil {
		if ctx.Err() != nil {
			log.Printf("auto-summary: chat=%d cancelled mid-inference", chatID)
		} else {
			log.Printf("auto-summary: %v", err)
		}
		return
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return
	}

	newMsgs := []store.Message{
		{Role: "user", Content: autoSummaryMarker + " " + summary},
	}
	newMsgs = append(newMsgs, keep...)

	lock.Lock()
	defer lock.Unlock()
	if err := b.store.Save(chatID, newMsgs); err != nil {
		log.Printf("auto-summary save: %v", err)
		return
	}
	log.Printf("auto-summary: chat=%d compressed %d→1 msgs", chatID, len(toCompress))
}

// estimateTokens approximates the token count of a single message body using
// llama.cpp's rough heuristic (chars/4 + a small per-message overhead).
// Same formula used for history trimming and for the prompt-size warning.
func estimateTokens(content string) int {
	return (len(content)+3)/4 + 4
}

// estimateLLMTokens sums estimateTokens across every message destined for
// the model so we can warn before we exceed the context window.
func estimateLLMTokens(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		n += estimateTokens(m.Content)
	}
	return n
}

// trimHistoryByTokens returns the most-recent suffix of msgs whose total
// estimated token count fits inside budget. If the first message is the
// auto-summary marker it is always preserved (its cost is deducted first).
func trimHistoryByTokens(msgs []store.Message, budget int) []store.Message {
	if len(msgs) == 0 {
		return msgs
	}
	var summary *store.Message
	if strings.HasPrefix(msgs[0].Content, autoSummaryMarker) {
		s := msgs[0]
		summary = &s
		budget -= estimateTokens(s.Content)
		if budget < 200 {
			budget = 200
		}
		msgs = msgs[1:]
	}
	total := 0
	var trimmed []store.Message
	for i := len(msgs) - 1; i >= 0; i-- {
		cost := estimateTokens(msgs[i].Content)
		if total+cost > budget {
			trimmed = msgs[i+1:]
			break
		}
		total += cost
	}
	if trimmed == nil {
		trimmed = msgs
	}
	if summary != nil {
		return append([]store.Message{*summary}, trimmed...)
	}
	return trimmed
}

func (b *Bot) Run(ctx context.Context) error {
	b.rootCtx = ctx
	updates, err := b.tg.UpdatesViaLongPolling(ctx, nil)
	if err != nil {
		return err
	}
	log.Printf("bot @%s started", b.username)

	go b.reminderLoop(ctx)

	for upd := range updates {
		if upd.Message == nil {
			continue
		}
		go b.handle(ctx, upd.Message)
	}
	return nil
}

// reminderLoop polls the reminders table every 30s. Each due reminder is
// posted to its chat and deleted. Runs until ctx is cancelled.
func (b *Bot) reminderLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	check := func() {
		due, err := b.store.DueReminders(time.Now())
		if err != nil {
			log.Printf("reminder loop: %v", err)
			return
		}
		for _, r := range due {
			msg := b.msgs.Get().UI.ReminderPrefix + r.Text
			if _, err := b.tg.SendMessage(ctx, tu.Message(tu.ID(r.ChatID), msg)); err != nil {
				log.Printf("reminder send: chat=%d id=%d err=%v", r.ChatID, r.ID, err)
				// Leave it in the table so we try again next tick.
				continue
			}
			if err := b.store.DeleteReminder(r.ID); err != nil {
				log.Printf("reminder delete: id=%d err=%v", r.ID, err)
			}
			b.appendMessage(r.ChatID, store.Message{Role: "assistant", Content: msg})
			log.Printf("reminder fired: chat=%d id=%d", r.ChatID, r.ID)
		}
	}
	check()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}

func (b *Bot) handle(ctx context.Context, msg *telego.Message) {
	if msg.From == nil {
		return
	}

	isGroup := msg.Chat.Type != telego.ChatTypePrivate

	// Membership bookkeeping runs before the text early-return because join/
	// leave service messages usually carry no text.
	if isGroup {
		if len(msg.NewChatMembers) > 0 {
			for _, u := range msg.NewChatMembers {
				if u.Username == b.username {
					continue
				}
				if err := b.store.UpsertMember(msg.Chat.ID, u.ID, u.FirstName, u.Username); err != nil {
					log.Printf("upsert join: %v", err)
				} else {
					log.Printf("member joined: chat=%d user=%d (%s)", msg.Chat.ID, u.ID, u.Username)
				}
			}
		}
		if msg.LeftChatMember != nil && msg.LeftChatMember.Username != b.username {
			if err := b.store.RemoveMember(msg.Chat.ID, msg.LeftChatMember.ID); err != nil {
				log.Printf("remove leave: %v", err)
			} else {
				log.Printf("member left: chat=%d user=%d (%s)",
					msg.Chat.ID, msg.LeftChatMember.ID, msg.LeftChatMember.Username)
			}
		}
		if msg.From.Username != b.username {
			if err := b.store.UpsertMember(msg.Chat.ID, msg.From.ID, msg.From.FirstName, msg.From.Username); err != nil {
				log.Printf("upsert sender: %v", err)
			}
		}
	}

	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}

	log.Printf("recv: chat=%d (%s) from=%d (%s) text=%q",
		msg.Chat.ID, msg.Chat.Type, msg.From.ID, msg.From.Username, textutil.Ellipsize(text, 80))

	// In DMs every message addresses the bot. In groups: a slash command, a
	// reply to one of the bot's messages, or an explicit @mention.
	isCommand := strings.HasPrefix(text, "/")
	addressed := !isGroup
	if isGroup {
		if isCommand {
			addressed = true
		}
		if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil &&
			msg.ReplyToMessage.From.Username == b.username {
			addressed = true
		}
		tag := "@" + b.username
		if idx := strings.Index(strings.ToLower(text), strings.ToLower(tag)); idx >= 0 {
			addressed = true
			text = strings.TrimSpace(text[:idx] + text[idx+len(tag):])
		}
	}

	userAllowed := b.cfg.AllowedUserIDs[msg.From.ID]
	chatAllowed := isGroup && b.cfg.AllowedChatIDs[msg.Chat.ID]
	if !userAllowed && !chatAllowed {
		if addressed {
			_, _ = b.tg.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), b.msgs.Get().UI.Unauthorized))
			log.Printf("rejected user %d (%s) in chat %d", msg.From.ID, msg.From.Username, msg.Chat.ID)
		}
		return
	}

	if !isCommand {
		b.appendMessage(msg.Chat.ID, store.Message{
			Role:    "user",
			Content: fmt.Sprintf("[%s] %s", b.speakerName(msg.From), text),
		})
		if isGroup && !addressed {
			go b.tryReact(b.rootCtx, msg)
			if b.tickSpontaneous(msg.Chat.ID) {
				go b.spontaneousReply(b.rootCtx, msg.Chat.ID)
			}
		}
		b.maybeAutoSummarize(msg.Chat.ID)
	}

	if !addressed {
		return
	}

	if isCommand {
		cmd, _, _ := strings.Cut(text, " ")
		cmd, _, _ = strings.Cut(cmd, "@")
		m := b.msgs.Get()
		switch cmd {
		case "/start":
			_, _ = b.tg.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), m.UI.Start))
			return
		case "/reset":
			_ = b.store.Clear(msg.Chat.ID)
			_, _ = b.tg.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), m.UI.MemoryCleared))
			return
		case "/status":
			_, _ = b.tg.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID),
				fmt.Sprintf(m.UI.StatusFormat, b.cfg.ModelFile, msg.From.ID, msg.Chat.ID)))
			return
		case "/summary":
			go b.onDemandSummary(b.rootCtx, msg.Chat.ID)
			return
		case "/help":
			_, _ = b.tg.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), m.UI.Help))
			return
		}
		// Unknown slash command — fall through and let the model see it.
	}

	// User addressed the bot — pre-empt any background summary so we serve
	// the user promptly instead of making them wait minutes behind it.
	b.cancelAutoSummary(msg.Chat.ID)

	if err := b.respond(ctx, msg); err != nil {
		log.Printf("respond: %v", err)
		_, _ = b.tg.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), b.msgs.Get().UI.GenericError))
	}
}

func (b *Bot) respond(ctx context.Context, msg *telego.Message) error {
	m := b.msgs.Get()
	if !b.claimInflight(msg.Chat.ID) {
		log.Printf("skip: chat=%d already has an inference in flight", msg.Chat.ID)
		_, _ = b.tg.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), m.UI.BusyPrevious))
		return nil
	}
	defer b.releaseInflight(msg.Chat.ID)

	history, err := b.store.Load(msg.Chat.ID)
	if err != nil {
		return fmt.Errorf("load history: %w", err)
	}
	// Capture the just-archived user message so we can re-feed it after a
	// context-overflow auto-reset.
	var lastUserMsg *store.Message
	if n := len(history); n > 0 && history[n-1].Role == "user" {
		um := history[n-1]
		lastUserMsg = &um
	}
	history = trimHistoryByTokens(history, b.cfg.HistoryTokenBudget)
	llmMsgs := buildLLMMessages(b.systemPromptForChat(msg.Chat.ID), history)

	placeholder, err := b.tg.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), m.UI.ThinkingPlaceholder))
	if err != nil {
		return fmt.Errorf("send placeholder: %w", err)
	}

	typingCtx, stopTyping := context.WithCancel(ctx)
	defer stopTyping()
	go b.keepTyping(typingCtx, msg.Chat.ID)

	s := newStreamer(ctx, b.tg, b.msgs, msg.Chat.ID, placeholder.MessageID, b.cfg.StreamInterval)
	defer s.Close()

	turnCtx := tools.WithChat(ctx, msg.Chat.ID, msg.From.ID)

	var final string
	overflowFailures := 0
	for {
		var runErr error
		final, runErr = b.runTurn(turnCtx, msg.Chat.ID, llmMsgs, s)
		if runErr == nil {
			break
		}
		if !isContextOverflow(runErr) {
			log.Printf("runTurn: chat=%d err=%v", msg.Chat.ID, runErr)
			s.replace(m.UI.GenericError)
			return nil
		}
		overflowFailures++
		log.Printf("context overflow: chat=%d failure=%d, resetting live history", msg.Chat.ID, overflowFailures)
		_ = b.store.Clear(msg.Chat.ID)
		if overflowFailures > maxContextOverflowRetries || lastUserMsg == nil {
			s.replace(m.UI.ContextOverflowGiveUp)
			return nil
		}
		// Re-feed the user message into the freshly-cleared live history and retry.
		b.appendMessage(msg.Chat.ID, *lastUserMsg)
		llmMsgs = buildLLMMessages(b.systemPromptForChat(msg.Chat.ID), []store.Message{*lastUserMsg})
		s.reset()
	}
	if strings.TrimSpace(final) == "" {
		final = m.UI.EmptyResponse
	}
	final = textutil.Ellipsize(final, maxMsgLen)
	log.Printf("reply: chat=%d text=%q", msg.Chat.ID, textutil.Ellipsize(final, 200))
	s.replace(final)

	b.appendMessage(msg.Chat.ID, store.Message{Role: "assistant", Content: final})
	return nil
}

// isContextOverflow reports whether err is llama-server's
// exceed_context_size_error (returned when the prompt is larger than -c).
func isContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "exceed_context_size_error") ||
		strings.Contains(msg, "exceeds the available context size")
}

// warnIfContextLarge logs whenever a prompt approaches the 4096-token cap
// and posts a one-shot Telegram warning to the chat (rate-limited per chat)
// so the user knows old context is at risk of being silently truncated.
func (b *Bot) warnIfContextLarge(ctx context.Context, chatID int64, msgs []llm.Message) {
	tokens := estimateLLMTokens(msgs)
	if tokens < b.cfg.ContextWarnThreshold {
		return
	}
	log.Printf("context warn: chat=%d est_tokens=%d threshold=%d (llama-server -c is 4096)",
		chatID, tokens, b.cfg.ContextWarnThreshold)

	b.contextWarnMu.Lock()
	last, ok := b.contextWarnAt[chatID]
	if ok && time.Since(last) < b.cfg.ContextWarnCooldown {
		b.contextWarnMu.Unlock()
		return
	}
	b.contextWarnAt[chatID] = time.Now()
	b.contextWarnMu.Unlock()

	msg := fmt.Sprintf(b.msgs.Get().UI.ContextWarnFormat, tokens)
	if _, err := b.tg.SendMessage(ctx, tu.Message(tu.ID(chatID), msg)); err != nil {
		log.Printf("context warn send: %v", err)
	}
}

func (b *Bot) runTurn(ctx context.Context, chatID int64, msgs []llm.Message, s *streamer) (string, error) {
	defs := b.tools.Definitions()
	for round := 0; round < maxToolRounds; round++ {
		b.warnIfContextLarge(ctx, chatID, msgs)
		text, calls, err := b.llm.Chat(ctx, msgs, defs, s)
		if err != nil {
			return "", err
		}
		if len(calls) == 0 {
			return text, nil
		}
		msgs = append(msgs, llm.Message{
			Role:      "assistant",
			Content:   text,
			ToolCalls: calls,
		})
		for _, c := range calls {
			log.Printf("tool: %s(%s)", c.Function.Name, textutil.Ellipsize(c.Function.Arguments, 200))
			result := b.tools.Execute(ctx, c)
			msgs = append(msgs, llm.Message{
				Role:       "tool",
				ToolCallID: c.ID,
				Name:       c.Function.Name,
				Content:    result,
			})
		}
		s.reset()
	}
	return "", fmt.Errorf("exceeded %d tool-call rounds", maxToolRounds)
}

// keepTyping refreshes the Telegram "typing..." indicator every 4s until ctx
// is canceled. Telegram auto-clears the indicator after ~5s, so we have to
// re-send it to keep the "Bot is typing..." chip visible while we work.
func (b *Bot) keepTyping(ctx context.Context, chatID int64) {
	send := func() {
		_ = b.tg.SendChatAction(ctx, &telego.SendChatActionParams{
			ChatID: tu.ID(chatID),
			Action: telego.ChatActionTyping,
		})
	}
	send()
	ticker := time.NewTicker(4 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			send()
		}
	}
}

func (b *Bot) loadSystemPrompt() string {
	prompt := b.cachedSystemPrompt()
	return strings.ReplaceAll(prompt, "{date}", time.Now().UTC().Format("2006-01-02"))
}

// systemPromptForChat returns the base system prompt with a per-chat
// "Miembros conocidos:" suffix listing the names the bot uses for each known
// member — the same labels that appear inside `[Name] message` envelopes, so
// the model can tie a name in the list to the speaker tag.
func (b *Bot) systemPromptForChat(chatID int64) string {
	prompt := b.loadSystemPrompt()
	members, err := b.store.ListMembers(chatID)
	if err != nil || len(members) == 0 {
		return prompt
	}
	seen := map[string]bool{}
	names := make([]string, 0, len(members))
	for _, m := range members {
		name := sanitizeSpeaker(b.loadUserNameOverride(m.UserID))
		if name == "" {
			name = sanitizeSpeaker(m.FirstName)
		}
		if name == "" {
			name = sanitizeSpeaker(m.Username)
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	if len(names) == 0 {
		return prompt
	}
	return prompt + "\n\n" + b.msgs.Get().UI.ChatMembersPrefix + strings.Join(names, ", ") + "."
}

// cachedSystemPrompt returns the system-prompt file content, re-reading only
// when the file's mtime changes. Falls back to the sibling .example.txt so a
// fresh clone runs without the user having to copy the file first.
func (b *Bot) cachedSystemPrompt() string {
	path := resolveConfigPath(b.cfg.SystemPromptPath, ".example.txt")
	if path == "" {
		return "You are a helpful assistant."
	}
	info, err := os.Stat(path)
	if err != nil {
		return "You are a helpful assistant."
	}
	mtime := info.ModTime()
	b.promptMu.RLock()
	if !mtime.After(b.promptMtime) && b.promptCached != "" {
		s := b.promptCached
		b.promptMu.RUnlock()
		return s
	}
	b.promptMu.RUnlock()

	raw, err := os.ReadFile(path)
	if err != nil {
		return "You are a helpful assistant."
	}
	s := string(raw)
	b.promptMu.Lock()
	b.promptCached = s
	b.promptMtime = mtime
	b.promptMu.Unlock()
	return s
}

// resolveConfigPath returns p if it exists; otherwise returns the
// example-sibling path (basename without trailing ext + exampleSuffix) in the
// same directory, if THAT exists. Returns "" if neither exists.
//
// Example: ("/config/system-prompt.txt", ".example.txt") →
//   "/config/system-prompt.txt"           if it exists
//   "/config/system-prompt.example.txt"   otherwise, if THAT exists
//   ""                                    otherwise
func resolveConfigPath(p, exampleSuffix string) string {
	if _, err := os.Stat(p); err == nil {
		return p
	}
	dir := filepath.Dir(p)
	base := filepath.Base(p)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	candidate := filepath.Join(dir, stem+exampleSuffix)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return ""
}

func (s *streamer) gerundFor(phaseStart time.Time) string {
	gs := s.msgs.Get().UI.BusyGerunds
	if len(gs) == 0 {
		return "Thinking"
	}
	// Change roughly every 4 seconds within a phase, deterministically.
	idx := int(time.Since(phaseStart).Seconds()) / 4
	return gs[idx%len(gs)]
}

func (s *streamer) toolPhrase(name string) string {
	ui := s.msgs.Get().UI
	if p, ok := ui.ToolPhrases[name]; ok && p != "" {
		return p
	}
	if strings.Contains(ui.ToolPhraseDefault, "%s") {
		return fmt.Sprintf(ui.ToolPhraseDefault, name)
	}
	return "Running " + name
}

type streamerState int

const (
	stateBusy streamerState = iota
	stateTool
	stateText
)

// streamer manages bot output during one turn: the "placeholder" message shows
// status (Pensando..., Searching the web...) while one or more "text" messages
// carry the streamed content. Each content phase (initial, post-tool-1, ...)
// gets its own freshly-sent message so Telegram's per-message edit rate-limit
// can't truncate the final reply.
type streamer struct {
	ctx           context.Context
	tg            *telego.Bot
	msgs          *messages.Loader
	chatID        int64
	placeholderID int // status indicator — edited by renderStatus only
	messageID     int // current text target; == placeholderID ⇒ next text chunk starts a new message

	streamInterval time.Duration // minimum gap between text edits (rate-limit safety)

	mu         sync.Mutex
	state      streamerState
	buf        strings.Builder
	phaseStart time.Time
	toolName   string
	lastFlush  time.Time

	editMu     sync.Mutex
	lastSent   string // last text written to the content message
	lastStatus string // last text written to the placeholder

	done chan struct{}
}

func newStreamer(ctx context.Context, tg *telego.Bot, msgs *messages.Loader, chatID int64, messageID int, streamInterval time.Duration) *streamer {
	s := &streamer{
		ctx:            ctx,
		tg:             tg,
		msgs:           msgs,
		streamInterval: streamInterval,
		chatID:         chatID,
		placeholderID:  messageID,
		messageID:      messageID,
		state:          stateBusy,
		phaseStart:     time.Now(),
		lastFlush:      time.Now(),
		done:           make(chan struct{}),
	}
	go s.tickLoop()
	return s
}

// Status ticks edit the placeholder. Telegram rate-limits per-chat edit
// volume, and reasoning models can spend 30-60s in stateBusy before any
// visible text arrives — at 2s ticks that was ~20 edits on a single message
// per turn, which triggered 22s retry-after lockouts that then killed the
// final reply's edit/send. The gerund only rotates every 4s, so 6s ticks
// keep the visual cadence while cutting edit volume ~3×.
func (s *streamer) tickLoop() {
	ticker := time.NewTicker(6 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.renderStatus()
		}
	}
}

func (s *streamer) editStatus(text string) {
	if text == "" {
		return
	}
	text = textutil.Ellipsize(text, maxMsgLen)
	s.editMu.Lock()
	if text == s.lastStatus {
		s.editMu.Unlock()
		return
	}
	s.editMu.Unlock()
	_, err := s.tg.EditMessageText(s.ctx, &telego.EditMessageTextParams{
		ChatID:    tu.ID(s.chatID),
		MessageID: s.placeholderID,
		Text:      text,
	})
	if err != nil {
		errStr := err.Error()
		if !strings.Contains(errStr, "message is not modified") &&
			!strings.Contains(errStr, "Too Many Requests") {
			log.Printf("editStatus: msg=%d err=%v", s.placeholderID, err)
		}
		return
	}
	s.editMu.Lock()
	s.lastStatus = text
	s.editMu.Unlock()
}

func (s *streamer) renderStatus() {
	s.mu.Lock()
	state := s.state
	start := s.phaseStart
	tool := s.toolName
	s.mu.Unlock()

	elapsed := int(time.Since(start).Seconds())
	switch state {
	case stateBusy:
		s.editStatus(fmt.Sprintf("%s... (%ds)", s.gerundFor(start), elapsed))
	case stateTool:
		s.editStatus(fmt.Sprintf("%s... (%ds)", s.toolPhrase(tool), elapsed))
	}
}

func (s *streamer) OnText(delta string) {
	s.mu.Lock()
	// First non-blank delta of a content phase: send a brand-new Telegram
	// message so each phase has its own per-message edit budget. messageID
	// equal to placeholderID means we're at the start of a phase.
	if s.messageID == s.placeholderID && strings.TrimSpace(delta) != "" {
		s.state = stateText
		s.buf.Reset()
		s.buf.WriteString(delta)
		s.lastFlush = time.Now()
		s.mu.Unlock()
		sent, err := s.tg.SendMessage(s.ctx, tu.Message(tu.ID(s.chatID), delta))
		if err != nil {
			// 429 on sendMessage falls back to editing the placeholder so
			// the text isn't dropped. messageID stays at placeholderID.
			log.Printf("OnText send (falling back to edit): %v", err)
			s.editMu.Lock()
			s.lastSent = ""
			s.editMu.Unlock()
			return
		}
		s.mu.Lock()
		s.messageID = sent.MessageID
		s.editMu.Lock()
		s.lastSent = delta
		s.editMu.Unlock()
		s.mu.Unlock()
		return
	}
	s.state = stateText
	s.buf.WriteString(delta)
	due := time.Since(s.lastFlush) >= s.streamInterval
	s.mu.Unlock()
	if due {
		s.flush()
	}
}

func (s *streamer) OnToolStart(name string) {
	s.mu.Lock()
	s.state = stateTool
	s.toolName = name
	s.phaseStart = time.Now()
	s.mu.Unlock()
	s.renderStatus()
}

// reset signals the start of a new content phase (after a tool round).
// Setting messageID back to placeholderID tells OnText/replace to send a new
// Telegram message for the next text instead of editing the previous one.
func (s *streamer) reset() {
	s.mu.Lock()
	s.buf.Reset()
	s.state = stateBusy
	s.phaseStart = time.Now()
	s.toolName = ""
	s.messageID = s.placeholderID
	s.editMu.Lock()
	s.lastSent = ""
	s.editMu.Unlock()
	s.mu.Unlock()
}

// replace delivers the final, canonical text. Must always land: if the model
// streamed nothing, send a fresh message; if the streaming edit-budget is
// exhausted, fall back to sending the full text as a new follow-up message
// so the user is never left looking at a truncated streamed chunk.
func (s *streamer) replace(text string) {
	s.mu.Lock()
	needsNew := s.messageID == s.placeholderID
	s.state = stateText
	// Clear buf so the deferred Close()→flush() doesn't overwrite text with
	// a stale streaming chunk.
	s.buf.Reset()
	s.mu.Unlock()
	if needsNew {
		if s.sendNewText(text) {
			return
		}
		log.Printf("replace: send failed; falling back to editing placeholder")
	}
	if s.editTo(text) {
		return
	}
	log.Printf("replace: edit budget exhausted; sending follow-up message")
	if !s.sendNewText(text) {
		log.Printf("replace: follow-up send also failed; user may not see final text")
	}
}

func (s *streamer) sendNewText(text string) bool {
	sent, err := s.tg.SendMessage(s.ctx, tu.Message(tu.ID(s.chatID), text))
	if err != nil {
		log.Printf("sendNewText: %v", err)
		return false
	}
	s.mu.Lock()
	s.messageID = sent.MessageID
	s.editMu.Lock()
	s.lastSent = text
	s.editMu.Unlock()
	s.mu.Unlock()
	return true
}

func (s *streamer) Close() {
	close(s.done)
	s.flush()
}

func (s *streamer) flush() {
	s.mu.Lock()
	cur := s.buf.String()
	s.mu.Unlock()
	if cur == "" {
		return
	}
	s.editTo(cur)
	s.mu.Lock()
	s.lastFlush = time.Now()
	s.mu.Unlock()
}

// editTo returns true when the edit (eventually) landed. On 429 it honors
// Telegram's "retry after N" hint instead of guessing.
func (s *streamer) editTo(text string) bool {
	s.editMu.Lock()
	defer s.editMu.Unlock()
	if text == s.lastSent {
		return true
	}
	text = textutil.Ellipsize(text, maxMsgLen)
	for attempt := 0; attempt < editMaxAttempts; attempt++ {
		_, err := s.tg.EditMessageText(s.ctx, &telego.EditMessageTextParams{
			ChatID:    tu.ID(s.chatID),
			MessageID: s.messageID,
			Text:      text,
		})
		if err == nil {
			s.lastSent = text
			return true
		}
		if wait := parseRetryAfter(err); wait > 0 {
			if wait > editMaxRetryWait {
				log.Printf("editTo: msg=%d 429 retry-after=%s exceeds cap; skipping",
					s.messageID, wait)
				return false
			}
			log.Printf("editTo: msg=%d 429, retry after %s (attempt %d/%d)",
				s.messageID, wait, attempt+1, editMaxAttempts)
			select {
			case <-s.ctx.Done():
				return false
			case <-time.After(wait):
			}
			continue
		}
		log.Printf("editTo: msg=%d attempt=%d err=%v text=%q",
			s.messageID, attempt, err, textutil.Ellipsize(text, 80))
		return false
	}
	log.Printf("editTo: msg=%d gave up after %d attempts", s.messageID, editMaxAttempts)
	return false
}

// retryAfterRE matches Telegram's "retry after N" hint embedded in 429 errors.
var retryAfterRE = regexp.MustCompile(`retry after (\d+)`)

func parseRetryAfter(err error) time.Duration {
	if err == nil {
		return 0
	}
	m := retryAfterRE.FindStringSubmatch(err.Error())
	if m == nil {
		// Generic 429 without a hint — still back off a bit.
		if strings.Contains(err.Error(), "Too Many Requests") {
			return 2 * time.Second
		}
		return 0
	}
	n, e := strconv.Atoi(m[1])
	if e != nil || n <= 0 {
		return 2 * time.Second
	}
	return time.Duration(n) * time.Second
}

// Resolution order: user-names.json override, FirstName, Username, "user".
// The override file is hot-reloaded so edits take effect without a restart.
func (b *Bot) speakerName(u *telego.User) string {
	if u == nil {
		return "user"
	}
	if name := sanitizeSpeaker(b.loadUserNameOverride(u.ID)); name != "" {
		return name
	}
	if name := sanitizeSpeaker(u.FirstName); name != "" {
		return name
	}
	if name := sanitizeSpeaker(u.Username); name != "" {
		return name
	}
	return "user"
}

// sanitizeSpeaker strips characters that would let a user-controlled name
// break out of the "[Name] message" envelope and inject instructions into
// the model — newlines, brackets, and stray whitespace. Caps length so a
// pathological name can't dominate the prompt.
func sanitizeSpeaker(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t', '[', ']':
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}

func (b *Bot) loadUserNameOverride(id int64) string {
	m := b.cachedUserNames()
	return strings.TrimSpace(m[strconv.FormatInt(id, 10)])
}

func (b *Bot) cachedUserNames() map[string]string {
	info, err := os.Stat(b.cfg.UserNamesPath)
	if err != nil {
		return nil
	}
	mtime := info.ModTime()
	b.namesMu.RLock()
	if !mtime.After(b.namesMtime) && b.namesCached != nil {
		m := b.namesCached
		b.namesMu.RUnlock()
		return m
	}
	b.namesMu.RUnlock()

	raw, err := os.ReadFile(b.cfg.UserNamesPath)
	if err != nil {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	b.namesMu.Lock()
	b.namesCached = m
	b.namesMtime = mtime
	b.namesMu.Unlock()
	return m
}
