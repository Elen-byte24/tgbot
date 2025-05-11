package main

import (
 "log"
 "os"
 "strings"

 tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func main() {
 // Чтение токена из файла
 tokenBytes, err := os.ReadFile("token.txt")
 if err != nil {
  log.Fatalf("Ошибка при чтении token.txt: %v", err)
 }
 botToken := strings.TrimSpace(string(tokenBytes))

 // Инициализация бота
 bot, err := tgbotapi.NewBotAPI(botToken)
 if err != nil {
  log.Panic(err)
 }

 log.Printf("Бот авторизован как %s", bot.Self.UserName)

 // Настройка получения обновлений
 u := tgbotapi.NewUpdate(0)
 u.Timeout = 60
 updates := bot.GetUpdatesChan(u)

 // Обработка сообщений
 for update := range updates {
  if update.Message == nil {
   continue
  }

  msg := tgbotapi.NewMessage(update.Message.Chat.ID , update.Message.Text)
  bot.Send(msg)
 }
}
