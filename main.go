package main

import (
	"context"
	"log"

	"tuwunel-reset-oidc-bot/config"

	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: .env file not found")
	}

	cfg := config.Load()

	bot, err := NewBot(cfg)
	if err != nil {
		log.Fatalf("Failed to create bot: %v", err)
	}

	if err := bot.Start(context.Background()); err != nil {
		log.Fatalf("Failed to start bot: %v", err)
	}

	select {}
}
