package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"llm-telegram-bot/internal/bot"
	"llm-telegram-bot/internal/config"
	"llm-telegram-bot/internal/llm"
	"llm-telegram-bot/internal/store"
	"llm-telegram-bot/internal/tools"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer db.Close()

	llmClient := llm.New(cfg.LlamaBaseURL)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Printf("waiting for llama-server at %s ...", cfg.LlamaBaseURL)
	if err := llmClient.WaitReady(ctx, 10*time.Minute); err != nil {
		log.Fatalf("llama-server never became ready: %v", err)
	}
	log.Println("llama-server ready")

	registry := tools.New(cfg.SearxngURL, cfg.DataAPIURL)

	b, err := bot.New(cfg, db, llmClient, registry)
	if err != nil {
		log.Fatalf("bot: %v", err)
	}

	if err := b.Run(ctx); err != nil {
		log.Fatalf("run: %v", err)
	}
}
