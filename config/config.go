package config

import (
	"os"
)

type Config struct {
	Homeserver   string
	BotUsername  string
	BotPassword  string
	AccessToken  string
	DeviceID     string
	PickleKey    string
	CryptoDBPath string
	DisplayName  string
	AvatarURL    string
	AdminRoomID  string
	Debug        bool
}

func Load() *Config {
	return &Config{
		Homeserver:   getEnv("MATRIX_HOMESERVER", "https://matrix.gubanov.site"),
		BotUsername:  getEnv("MATRIX_BOT_USERNAME", ""),
		BotPassword:  getEnv("MATRIX_BOT_PASSWORD", ""),
		AccessToken:  getEnv("MATRIX_ACCESS_TOKEN", ""),
		DeviceID:     getEnv("MATRIX_DEVICE_ID", ""),
		PickleKey:    getEnv("MATRIX_PICKLE_KEY", "your-secret-pickle-key-change-me"),
		CryptoDBPath: getEnv("MATRIX_CRYPTO_DB", "crypto.db"),
		DisplayName:  getEnv("MATRIX_BOT_DISPLAYNAME", "Password Reset Bot"),
		AvatarURL:    getEnv("MATRIX_BOT_AVATAR", ""),
		AdminRoomID:  getEnv("MATRIX_ADMIN_ROOM_ID", ""),
		Debug:        getEnv("DEBUG", "false") == "true",
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
