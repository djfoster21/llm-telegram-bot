package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"llm-telegram-bot/internal/config"
	"llm-telegram-bot/internal/llm"
	"llm-telegram-bot/internal/store"
	"llm-telegram-bot/internal/tools"
)

const (
	maxToolRounds     = 4
	streamInterval    = 1100 * time.Millisecond
	maxMsgLen         = 4000
	maxStoredMessages = 300 // absolute cap on per-chat rows to bound disk growth

	// Spontaneous-take frequency window: every N user messages (in a group),
	// chosen uniformly from [min, max], the bot may chime in unprompted.
	spontaneousMin = 1
	spontaneousMax = 15

	// Auto-summarization: when stored messages exceed this count, compress the
	// oldest portion into a single "[CONTEXTO ANTERIOR]" user message so the
	// bot keeps long-term context without blowing the token budget.
	autoSummaryAtMessages = 60
	autoSummaryKeepRecent = 30
	autoSummaryMarker     = "[CONTEXTO ANTERIOR]"
	autoSummaryCooldown   = 30 * time.Minute

	// Probability of reacting to a message that matches a pattern. Keeps the
	// bot from spamming a 🤣 on every "jaja" while still feeling alive.
	reactionProbability = 0.4

	// Generic message shown to chat on internal errors. The full error is
	// logged; we don't leak it to users because it can carry internal
	// hostnames or paths.
	genericErrorMsg = "Uy, se me colgó algo. Probá de nuevo."
)

// reactionPatterns maps regex matches on message text to one of Telegram's
// allowed reaction emojis. Order matters — first match wins.
var reactionPatterns = []struct {
	re    *regexp.Regexp
	emoji string
}{
	{regexp.MustCompile(`(?i)^(ja|je|ji|jo){2,}|^(lol|lmao|kek)\b`), "🤣"},
	{regexp.MustCompile(`(?i)\b(posta|joya|copado|tremendo|capo|capazo|crack|ídolo|idolo)\b`), "🔥"},
	{regexp.MustCompile(`(?i)\b(rip|murió|murio|me muero|estoy muerto|finado)\b`), "😱"},
	{regexp.MustCompile(`(?i)\b(claro|obvio|exacto|tal cual|exactamente|100%)\b`), "👍"},
	{regexp.MustCompile(`(?i)\b(genial|increíble|increible|brutal|hermoso)\b`), "🤩"},
	{regexp.MustCompile(`(?i)\b(qu[eé] pelotudo|gil|forro|salam[ií]n)\b`), "🤡"},
	{regexp.MustCompile(`(?i)\b(no entiendo|qu[eé]\s?\?|c[oó]mo\?|wat)\b`), "🤔"},
}

type Bot struct {
	cfg      *config.Config
	tg       *telego.Bot
	store    *store.Store
	llm      *llm.Client
	tools    *tools.Registry
	username string

	inflightMu sync.Mutex
	inflight   map[int64]bool

	// per-chat write mutex so concurrent archive + respond don't race on the
	// load → modify → save pattern.
	chatLocks sync.Map // map[int64]*sync.Mutex

	// per-chat spontaneous-reply state.
	spontMu   sync.Mutex
	spontData map[int64]*spontaneousState

	// per-chat auto-summarization state — prevents two background summarizers
	// from running on the same chat concurrently, enforces a cooldown so it
	// can't immediately re-fire, and tracks a cancel func so addressed user
	// messages can interrupt a running summary.
	summarizingMu     sync.Mutex
	summarizing       map[int64]bool
	lastSummaryAt     map[int64]time.Time
	summaryCancel     map[int64]context.CancelFunc
}

type spontaneousState struct {
	count     int
	threshold int
}

func New(cfg *config.Config, db *store.Store, llmClient *llm.Client, registry *tools.Registry) (*Bot, error) {
	tg, err := telego.NewBot(cfg.TelegramToken)
	if err != nil {
		return nil, err
	}
	me, err := tg.GetMe(context.Background())
	if err != nil {
		return nil, fmt.Errorf("get me: %w", err)
	}
	return &Bot{
		cfg:       cfg,
		tg:        tg,
		store:     db,
		llm:       llmClient,
		tools:     registry,
		username:  me.Username,
		inflight:      map[int64]bool{},
		spontData:     map[int64]*spontaneousState{},
		summarizing:   map[int64]bool{},
		lastSummaryAt: map[int64]time.Time{},
		summaryCancel: map[int64]context.CancelFunc{},
	}, nil
}

// claimInflight returns true if we acquired the per-chat inference lock,
// false if there's already an inference in flight for this chat.
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

// chatLock returns the per-chat write mutex for serializing history updates.
func (b *Bot) chatLock(chatID int64) *sync.Mutex {
	actual, _ := b.chatLocks.LoadOrStore(chatID, &sync.Mutex{})
	return actual.(*sync.Mutex)
}

// archiveMessage appends a user message to the chat's history. Used for both
// @mentions and non-mention group chatter so the LLM sees the full conversation.
func (b *Bot) archiveMessage(chatID int64, user *telego.User, text string) {
	lock := b.chatLock(chatID)
	lock.Lock()
	defer lock.Unlock()

	msgs, err := b.store.Load(chatID)
	if err != nil {
		log.Printf("archive load: %v", err)
		return
	}
	msgs = append(msgs, store.Message{
		Role:    "user",
		Content: fmt.Sprintf("[%s] %s", b.speakerName(user), text),
	})
	if len(msgs) > maxStoredMessages {
		msgs = msgs[len(msgs)-maxStoredMessages:]
	}
	if err := b.store.Save(chatID, msgs); err != nil {
		log.Printf("archive save: %v", err)
	}
}

// appendAssistant adds the model's reply to the chat's history. Run after
// inference completes; reloads the current history to capture any messages
// archived concurrently during inference.
func (b *Bot) appendAssistant(chatID int64, text string) {
	lock := b.chatLock(chatID)
	lock.Lock()
	defer lock.Unlock()

	msgs, err := b.store.Load(chatID)
	if err != nil {
		log.Printf("append load: %v", err)
		return
	}
	msgs = append(msgs, store.Message{Role: "assistant", Content: text})
	if len(msgs) > maxStoredMessages {
		msgs = msgs[len(msgs)-maxStoredMessages:]
	}
	if err := b.store.Save(chatID, msgs); err != nil {
		log.Printf("append save: %v", err)
	}
}

// tickSpontaneous increments the per-chat user-message counter and returns
// true if it crossed the random threshold (meaning: time for an unprompted
// take). Resets and picks a new threshold after firing.
func (b *Bot) tickSpontaneous(chatID int64) bool {
	b.spontMu.Lock()
	defer b.spontMu.Unlock()
	st, ok := b.spontData[chatID]
	if !ok {
		st = &spontaneousState{threshold: randomThreshold()}
		b.spontData[chatID] = st
	}
	st.count++
	log.Printf("spontaneous tick: chat=%d count=%d threshold=%d", chatID, st.count, st.threshold)
	if st.count >= st.threshold {
		st.count = 0
		st.threshold = randomThreshold()
		return true
	}
	return false
}

func randomThreshold() int {
	return spontaneousMin + rand.Intn(spontaneousMax-spontaneousMin+1)
}

// spontaneousReply fires an unprompted take based on the current chat history.
// Bails out silently if another inference is already running or if the model
// returns nothing useful.
func (b *Bot) spontaneousReply(ctx context.Context, chatID int64) {
	if !b.claimInflight(chatID) {
		return
	}
	defer b.releaseInflight(chatID)

	systemPrompt := b.loadSystemPrompt()
	history, err := b.store.Load(chatID)
	if err != nil {
		return
	}
	history = trimHistoryByTokens(history, b.cfg.HistoryTokenBudget)
	if len(history) == 0 {
		return
	}

	llmMsgs := []llm.Message{{Role: "system", Content: systemPrompt}}
	for _, m := range history {
		llmMsgs = append(llmMsgs, llm.Message{Role: m.Role, Content: m.Content})
	}
	llmMsgs = append(llmMsgs, llm.Message{
		Role: "user",
		Content: "[SISTEMA] Sumate a la conversación como un amigo más, sin que nadie te invite. " +
			"Reglas: (1) agarrate de algo CONCRETO que dijo uno de los últimos 2-3 mensajes — nombralo o citá lo que dijo. " +
			"(2) Aportá algo: una observación, una pregunta, un dato, una opinión, o una chicana LIVIANA. No insultos gratuitos. " +
			"(3) Una o dos oraciones, rioplatense (voseo), amigable. " +
			"(4) Nada de frases sueltas que no se conecten con lo que se está hablando. " +
			"(5) SOLO si arriba no hay nada para comentar (chat vacío o un único 'hola'), respondé con la palabra SKIP a secas.",
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
	if len(final) > maxMsgLen {
		final = final[:maxMsgLen] + "..."
	}
	if _, err := b.tg.SendMessage(ctx, tu.Message(tu.ID(chatID), final)); err != nil {
		log.Printf("spontaneous send: %v", err)
		return
	}
	log.Printf("spontaneous reply: chat=%d text=%q", chatID, truncate(final, 200))
	b.appendAssistant(chatID, final)
}

// tryReact emits a Telegram message reaction if msg.Text matches one of the
// reaction patterns and a random gate fires. Best-effort, async-safe.
func (b *Bot) tryReact(ctx context.Context, msg *telego.Message) {
	if msg == nil || msg.Text == "" {
		return
	}
	for _, p := range reactionPatterns {
		if !p.re.MatchString(msg.Text) {
			continue
		}
		if rand.Float64() > reactionProbability {
			return
		}
		err := b.tg.SetMessageReaction(ctx, &telego.SetMessageReactionParams{
			ChatID:    tu.ID(msg.Chat.ID),
			MessageID: msg.MessageID,
			Reaction: []telego.ReactionType{
				&telego.ReactionTypeEmoji{Type: "emoji", Emoji: p.emoji},
			},
		})
		if err != nil {
			log.Printf("react: emoji=%s msg=%d text=%q err=%v",
				p.emoji, msg.MessageID, truncate(msg.Text, 60), err)
			return
		}
		log.Printf("react: chat=%d msg=%d emoji=%s", msg.Chat.ID, msg.MessageID, p.emoji)
		return
	}
}

// onDemandSummary handles /summary — replies in chat with a short summary of
// the current history. Doesn't modify stored history.
func (b *Bot) onDemandSummary(ctx context.Context, chatID int64) {
	if !b.claimInflight(chatID) {
		_, _ = b.tg.SendMessage(ctx, tu.Message(tu.ID(chatID),
			"Esperá che, todavía estoy pensando en otra cosa."))
		return
	}
	defer b.releaseInflight(chatID)

	history, err := b.store.Load(chatID)
	if err != nil {
		log.Printf("summary load: %v", err)
		return
	}
	if len(history) < 3 {
		_, _ = b.tg.SendMessage(ctx, tu.Message(tu.ID(chatID),
			"Todavía no hay mucho para resumir, che."))
		return
	}
	history = trimHistoryByTokens(history, b.cfg.HistoryTokenBudget)

	llmMsgs := []llm.Message{{Role: "system", Content: b.loadSystemPrompt()}}
	for _, m := range history {
		llmMsgs = append(llmMsgs, llm.Message{Role: m.Role, Content: m.Content})
	}
	llmMsgs = append(llmMsgs, llm.Message{
		Role: "user",
		Content: "[SISTEMA] Resumí la conversación de arriba en 2-3 oraciones bien cortas, " +
			"castellano rioplatense. Mencioná de qué se está hablando y cualquier dato importante " +
			"(decisiones, planes, chistes recurrentes). Nada de saludo ni intro.",
	})

	log.Printf("summary: chat=%d generating", chatID)
	final, _, err := b.llm.Chat(ctx, llmMsgs, nil, nil)
	if err != nil {
		log.Printf("summary: %v", err)
		_, _ = b.tg.SendMessage(ctx, tu.Message(tu.ID(chatID), genericErrorMsg))
		return
	}
	final = strings.TrimSpace(final)
	if final == "" {
		final = "(nada para resumir)"
	}
	if len(final) > maxMsgLen {
		final = final[:maxMsgLen] + "..."
	}
	_, _ = b.tg.SendMessage(ctx, tu.Message(tu.ID(chatID), final))
	log.Printf("summary reply: chat=%d text=%q", chatID, truncate(final, 200))
}

// maybeAutoSummarize fires an async background summarizer if the stored
// history exceeds autoSummaryAtMessages, none is already running, and the
// per-chat cooldown has elapsed.
func (b *Bot) maybeAutoSummarize(chatID int64) {
	msgs, err := b.store.Load(chatID)
	if err != nil || len(msgs) < autoSummaryAtMessages {
		return
	}
	b.summarizingMu.Lock()
	if b.summarizing[chatID] {
		b.summarizingMu.Unlock()
		return
	}
	if last, ok := b.lastSummaryAt[chatID]; ok && time.Since(last) < autoSummaryCooldown {
		b.summarizingMu.Unlock()
		return
	}
	b.summarizing[chatID] = true
	b.lastSummaryAt[chatID] = time.Now()
	b.summarizingMu.Unlock()
	go b.autoSummarize(chatID)
}

// cancelAutoSummary stops a running auto-summary for this chat (if any) so
// that a freshly-arrived addressed user message can grab the inflight lock
// quickly. Safe to call when no summary is running.
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
	ctx, cancel := context.WithCancel(context.Background())
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
	if err != nil || len(msgs) < autoSummaryAtMessages {
		return
	}

	// Separate existing summary (if any) and recent tail; the middle gets compressed.
	var existingSummary *store.Message
	if strings.HasPrefix(msgs[0].Content, autoSummaryMarker) {
		s := msgs[0]
		existingSummary = &s
		msgs = msgs[1:]
	}
	if len(msgs) <= autoSummaryKeepRecent {
		return
	}
	toCompress := msgs[:len(msgs)-autoSummaryKeepRecent]
	keep := msgs[len(msgs)-autoSummaryKeepRecent:]

	llmMsgs := []llm.Message{{Role: "system", Content: b.loadSystemPrompt()}}
	if existingSummary != nil {
		llmMsgs = append(llmMsgs, llm.Message{Role: "user", Content: existingSummary.Content})
	}
	for _, m := range toCompress {
		llmMsgs = append(llmMsgs, llm.Message{Role: m.Role, Content: m.Content})
	}
	llmMsgs = append(llmMsgs, llm.Message{
		Role: "user",
		Content: "[SISTEMA] Resumí toda la conversación de arriba en 4-5 oraciones, " +
			"castellano rioplatense, conservando nombres, decisiones, planes y chistes recurrentes. " +
			"Nada de saludos ni intro — devolvé solo el resumen.",
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

// trimHistoryByTokens returns the most-recent suffix of msgs whose total
// estimated token count fits inside budget. Estimation: chars/4 + small
// per-message overhead. If the first message is the auto-summary marker, it
// is always preserved (its cost is deducted from the budget first).
func trimHistoryByTokens(msgs []store.Message, budget int) []store.Message {
	if len(msgs) == 0 {
		return msgs
	}
	var summary *store.Message
	if strings.HasPrefix(msgs[0].Content, autoSummaryMarker) {
		s := msgs[0]
		summary = &s
		budget -= (len(s.Content)+3)/4 + 4
		if budget < 200 {
			budget = 200
		}
		msgs = msgs[1:]
	}
	total := 0
	var trimmed []store.Message
	for i := len(msgs) - 1; i >= 0; i-- {
		cost := (len(msgs[i].Content)+3)/4 + 4
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
	updates, err := b.tg.UpdatesViaLongPolling(ctx, nil)
	if err != nil {
		return err
	}
	log.Printf("bot @%s started", b.username)

	for upd := range updates {
		if upd.Message == nil {
			continue
		}
		go b.handle(ctx, upd.Message)
	}
	return nil
}

func (b *Bot) handle(ctx context.Context, msg *telego.Message) {
	if msg.From == nil {
		return
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}

	isGroup := msg.Chat.Type != telego.ChatTypePrivate

	log.Printf("recv: chat=%d (%s) from=%d (%s) text=%q",
		msg.Chat.ID, msg.Chat.Type, msg.From.ID, msg.From.Username, truncate(text, 80))

	// Detect whether the bot is being directly addressed (so we know whether
	// to reply). In DMs every message addresses the bot. In groups: an explicit
	// @mention, a reply to one of the bot's messages, or a slash command.
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

	// Auth: user is on the user-allowlist, OR it's a group whose chat ID is allowlisted.
	userAllowed := b.cfg.AllowedUserIDs[msg.From.ID]
	chatAllowed := isGroup && b.cfg.AllowedChatIDs[msg.Chat.ID]
	if !userAllowed && !chatAllowed {
		// Stay silent for bystanders; only respond when they were trying to
		// address the bot directly.
		if addressed {
			_, _ = b.tg.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), "Not authorized."))
			log.Printf("rejected user %d (%s) in chat %d", msg.From.ID, msg.From.Username, msg.Chat.ID)
		}
		return
	}

	// Archive every regular message (commands are transient — not saved).
	if !isCommand && text != "" {
		b.archiveMessage(msg.Chat.ID, msg.From, text)
		// In groups, lightweight reactions and spontaneous-take counter run
		// on every non-addressed message. Reactions are pattern-based (no LLM
		// call); the spontaneous take fires when a random threshold trips.
		if isGroup && !addressed {
			go b.tryReact(context.Background(), msg)
			if b.tickSpontaneous(msg.Chat.ID) {
				go b.spontaneousReply(context.Background(), msg.Chat.ID)
			}
		}
		// Async background summarization once the chat grows past the
		// threshold. No-op if one is already running or the chat is short.
		b.maybeAutoSummarize(msg.Chat.ID)
	}

	// Group non-mention chatter: archived, but we don't reply.
	if !addressed {
		return
	}

	switch {
	case strings.HasPrefix(text, "/start"):
		_, _ = b.tg.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID),
			"Hi. Ask me anything — I can search the web when I need to.\n/reset to clear memory.\n/summary for a recap of the chat.\n/status for info."))
		return
	case strings.HasPrefix(text, "/reset"):
		_ = b.store.Clear(msg.Chat.ID)
		_, _ = b.tg.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), "Memory cleared."))
		return
	case strings.HasPrefix(text, "/status"):
		_, _ = b.tg.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID),
			fmt.Sprintf("Model: %s\nUser ID: %d\nChat ID: %d", b.cfg.ModelFile, msg.From.ID, msg.Chat.ID)))
		return
	case strings.HasPrefix(text, "/summary"):
		go b.onDemandSummary(context.Background(), msg.Chat.ID)
		return
	}

	if text == "" {
		return
	}

	// User addressed the bot — pre-empt any background summary so we serve
	// the user promptly instead of making them wait minutes behind it.
	b.cancelAutoSummary(msg.Chat.ID)

	if err := b.respond(ctx, msg, text); err != nil {
		log.Printf("respond: %v", err)
		_, _ = b.tg.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), genericErrorMsg))
	}
}

func (b *Bot) respond(ctx context.Context, msg *telego.Message, userText string) error {
	if !b.claimInflight(msg.Chat.ID) {
		log.Printf("skip: chat=%d already has an inference in flight", msg.Chat.ID)
		_, _ = b.tg.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID),
			"Esperá che, todavía estoy pensando en la anterior."))
		return nil
	}
	defer b.releaseInflight(msg.Chat.ID)

	systemPrompt := b.loadSystemPrompt()

	// The user's current message has already been archived by handle().
	history, err := b.store.Load(msg.Chat.ID)
	if err != nil {
		return fmt.Errorf("load history: %w", err)
	}
	history = trimHistoryByTokens(history, b.cfg.HistoryTokenBudget)

	llmMsgs := []llm.Message{{Role: "system", Content: systemPrompt}}
	for _, m := range history {
		llmMsgs = append(llmMsgs, llm.Message{Role: m.Role, Content: m.Content})
	}

	placeholder, err := b.tg.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), "Thinking..."))
	if err != nil {
		return fmt.Errorf("send placeholder: %w", err)
	}

	typingCtx, stopTyping := context.WithCancel(ctx)
	defer stopTyping()
	go b.keepTyping(typingCtx, msg.Chat.ID)

	s := newStreamer(ctx, b.tg, msg.Chat.ID, placeholder.MessageID)
	defer s.Close()

	final, err := b.runTurn(ctx, llmMsgs, s)
	if err != nil {
		log.Printf("runTurn: chat=%d err=%v", msg.Chat.ID, err)
		s.replace(genericErrorMsg)
		return nil
	}
	if strings.TrimSpace(final) == "" {
		final = "(empty response)"
	}
	if len(final) > maxMsgLen {
		final = final[:maxMsgLen] + "..."
	}
	log.Printf("reply: chat=%d text=%q", msg.Chat.ID, truncate(final, 200))
	s.replace(final)

	b.appendAssistant(msg.Chat.ID, final)
	return nil
}

func (b *Bot) runTurn(ctx context.Context, msgs []llm.Message, s *streamer) (string, error) {
	defs := b.tools.Definitions()
	for round := 0; round < maxToolRounds; round++ {
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
			log.Printf("tool: %s(%s)", c.Function.Name, truncate(c.Function.Arguments, 200))
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
	raw, err := os.ReadFile(b.cfg.SystemPromptPath)
	if err != nil {
		return "You are a helpful assistant."
	}
	prompt := string(raw)
	prompt = strings.ReplaceAll(prompt, "{date}", time.Now().UTC().Format("2006-01-02"))
	return prompt
}

// Rotating placeholders shown while the model is busy
var busyGerunds = []string{
	"Thinking",
	"Cogitating",
	"Pondering",
	"Ruminating",
	"Marinating",
	"Percolating",
	"Synthesizing",
	"Calibrating",
	"Confabulating",
	"Wrangling thoughts",
	"Noodling",
	"Crystallizing",
	"Mulling it over",
	"Sharpening pencils",
	"Consulting the oracle",
}

func gerundFor(phaseStart time.Time) string {
	// Change roughly every 4 seconds within a phase, deterministically.
	idx := int(time.Since(phaseStart).Seconds()) / 4
	return busyGerunds[idx%len(busyGerunds)]
}

func toolPhrase(name string) string {
	switch name {
	case "search_web":
		return "Searching the web"
	case "fetch_url":
		return "Reading the page"
	default:
		return "Running " + name
	}
}

type streamerState int

const (
	stateBusy streamerState = iota // waiting for first token / prompt eval
	stateTool                      // tool is executing
	stateText                      // tokens are streaming in
)

// streamer manages bot output during one turn: a "placeholder" message shows
// status (Thinking..., Searching the web...) while one or more "text" messages
// carry the actual streamed content. After every state change to text (initial
// or post-tool), the streamer sends a NEW Telegram message and edits that one
// instead of the placeholder, so each content phase gets its own edit budget
// and Telegram rate-limiting can't truncate the final reply.
type streamer struct {
	ctx           context.Context
	tg            *telego.Bot
	chatID        int64
	placeholderID int // status indicator — edited by renderStatus only
	messageID     int // current text target — placeholder until first text chunk

	mu              sync.Mutex
	state           streamerState
	buf             strings.Builder
	phaseStart      time.Time
	toolName        string
	lastFlush       time.Time
	needsNewMessage bool // true → next text chunk creates a new message

	editMu   sync.Mutex // serializes edit calls + lastSent
	lastSent string     // last text successfully sent to messageID

	done chan struct{}
}

func newStreamer(ctx context.Context, tg *telego.Bot, chatID int64, messageID int) *streamer {
	s := &streamer{
		ctx:             ctx,
		tg:              tg,
		chatID:          chatID,
		placeholderID:   messageID,
		messageID:       messageID,
		state:           stateBusy,
		phaseStart:      time.Now(),
		lastFlush:       time.Now(),
		needsNewMessage: true,
		done:            make(chan struct{}),
	}
	go s.tickLoop()
	return s
}

func (s *streamer) tickLoop() {
	ticker := time.NewTicker(2 * time.Second)
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

// editStatus updates the placeholder message used for status indicators.
// Has its own lastSent tracking so it doesn't interfere with text edits.
func (s *streamer) editStatus(text string) {
	if text == "" {
		return
	}
	if len(text) > maxMsgLen {
		text = text[:maxMsgLen] + "..."
	}
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
	}
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
		s.editStatus(fmt.Sprintf("%s... (%ds)", gerundFor(start), elapsed))
	case stateTool:
		s.editStatus(fmt.Sprintf("%s... (%ds)", toolPhrase(tool), elapsed))
	case stateText:
		// Text streaming flushes itself; nothing to do here.
	}
}

func (s *streamer) OnText(delta string) {
	s.mu.Lock()
	if s.needsNewMessage && strings.TrimSpace(delta) != "" {
		s.needsNewMessage = false
		s.state = stateText
		s.buf.Reset()
		s.buf.WriteString(delta)
		s.lastFlush = time.Now()
		s.mu.Unlock()
		// Send a fresh message for this text phase so it has its own edit
		// budget. Subsequent OnText deltas edit this new message. If send
		// fails (e.g. 429 — Telegram rate-limits sendMessage separately from
		// edits), fall back to editing the placeholder so we don't drop the
		// text. messageID stays pointed at the placeholder in that case.
		sent, err := s.tg.SendMessage(s.ctx, tu.Message(tu.ID(s.chatID), delta))
		if err != nil {
			log.Printf("OnText send (falling back to edit): %v", err)
			s.mu.Lock()
			s.editMu.Lock()
			s.lastSent = ""
			s.editMu.Unlock()
			s.mu.Unlock()
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
	due := time.Since(s.lastFlush) >= streamInterval
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

// reset is called after a tool round finishes; we go back to "busy" while the
// model thinks about the tool result, with a fresh phase timer. The next text
// chunk will be sent as a new message instead of editing the previous one.
func (s *streamer) reset() {
	s.mu.Lock()
	s.buf.Reset()
	s.state = stateBusy
	s.phaseStart = time.Now()
	s.toolName = ""
	s.needsNewMessage = true
	s.editMu.Lock()
	s.lastSent = ""
	s.editMu.Unlock()
	s.mu.Unlock()
}

// replace sets the final text. If no text message has been created yet (e.g.
// model returned without streaming any tokens), sends a new message; otherwise
// edits the current text message with the final canonical content.
func (s *streamer) replace(text string) {
	s.mu.Lock()
	needsNew := s.needsNewMessage
	s.needsNewMessage = false
	s.state = stateText
	s.buf.Reset()
	s.buf.WriteString(text)
	s.mu.Unlock()
	if needsNew {
		sent, err := s.tg.SendMessage(s.ctx, tu.Message(tu.ID(s.chatID), text))
		if err == nil {
			s.mu.Lock()
			s.messageID = sent.MessageID
			s.editMu.Lock()
			s.lastSent = text
			s.editMu.Unlock()
			s.mu.Unlock()
			return
		}
		log.Printf("replace send (falling back to edit): %v", err)
		s.editMu.Lock()
		s.lastSent = ""
		s.editMu.Unlock()
	}
	s.editTo(text)
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

func (s *streamer) editTo(text string) {
	s.editMu.Lock()
	defer s.editMu.Unlock()
	if text == s.lastSent {
		return
	}
	if len(text) > maxMsgLen {
		text = text[:maxMsgLen] + "..."
	}
	for attempt := 0; attempt < 3; attempt++ {
		_, err := s.tg.EditMessageText(s.ctx, &telego.EditMessageTextParams{
			ChatID:    tu.ID(s.chatID),
			MessageID: s.messageID,
			Text:      text,
		})
		if err == nil {
			s.lastSent = text
			return
		}
		// Telegram rate-limits message edits. On 429 ("Too Many Requests"),
		// sleep briefly and retry — losing the final edit leaves the chat
		// showing a truncated stream chunk (e.g. just "Ni").
		errStr := err.Error()
		if strings.Contains(errStr, "Too Many Requests") || strings.Contains(errStr, "retry after") {
			time.Sleep(time.Duration(1+attempt) * time.Second)
			continue
		}
		log.Printf("editTo: msg=%d attempt=%d err=%v text=%q",
			s.messageID, attempt, err, truncate(text, 80))
		return
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// speakerName returns a short label for a Telegram user used to tag messages
// forwarded to the LLM. Resolution order:
//  1. Override from the mapping file at cfg.UserNamesPath (JSON object
//     keyed by user ID as a string, e.g. {"123": "Pibe"}). Re-read each
//     call so edits take effect without a restart.
//  2. Telegram's FirstName.
//  3. Telegram's Username.
//  4. Literal "user".
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
	raw, err := os.ReadFile(b.cfg.UserNamesPath)
	if err != nil {
		return ""
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	return strings.TrimSpace(m[strconv.FormatInt(id, 10)])
}
