package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"golang.org/x/sync/errgroup"
)

// Конфигурация приложения
type Config struct {
	BotToken      string `json:"bot_token"`
	DBHost        string `json:"db_host"`
	DBPort        int    `json:"db_port"`
	DBUser        string `json:"db_user"`
	DBPassword    string `json:"db_password"`
	DBName        string `json:"db_name"`
	WebhookURL    string `json:"webhook_url"`
	WebhookPort   int    `json:"webhook_port"`
	WebhookCert   string `json:"webhook_cert"`
	WebhookKey    string `json:"webhook_key"`
	DebugMode     bool   `json:"debug_mode"`
}

// Модели данных
type (
	User struct {
		ID        int64     `json:"id"`
		Username  string    `json:"username"`
		FirstName string    `json:"first_name"`
		LastName  string    `json:"last_name"`
		CreatedAt time.Time `json:"created_at"`
		Settings  UserSettings `json:"settings"`
	}

	UserSettings struct {
		MorningReminder bool   `json:"morning_reminder"`
		TimeZone        string `json:"time_zone"`
		Language        string `json:"language"`
	}

	Task struct {
		ID          int       `json:"id"`
		UserID      int64     `json:"user_id"`
		Title       string    `json:"title"`
		Description string    `json:"description"`
		StartTime   time.Time `json:"start_time"`
		EndTime     time.Time `json:"end_time"`
		Priority    int       `json:"priority"`
		Category    string    `json:"category"`
		Completed   bool      `json:"completed"`
		CreatedAt   time.Time `json:"created_at"`
	}

	DaySchedule struct {
		Date      time.Time `json:"date"`
		UserID    int64     `json:"user_id"`
		Tasks     []Task    `json:"tasks"`
		ProductivityScore float64 `json:"productivity_score"`
	}
)

// Сервисы приложения
type (
	BotService struct {
		bot      *tgbotapi.BotAPI
		db       *sql.DB
		config   *Config
		scheduler *SchedulerService
	}

	SchedulerService struct {
		db       *sql.DB
	}

	NotificationService struct {
		bot *tgbotapi.BotAPI
		db  *sql.DB
	}
)

// Инициализация конфигурации
func loadConfig(path string) (*Config, error) {
	file, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения конфига: %w", err)
	}

	var config Config
	if err := json.Unmarshal(file, &config); err != nil {
		return nil, fmt.Errorf("ошибка парсинга конфига: %w", err)
	}

	return &config, nil
}

// Инициализация базы данных
func initDB(config *Config) (*sql.DB, error) {
	connStr := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
		config.DBHost, config.DBPort, config.DBUser, config.DBPassword, config.DBName)

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, fmt.Errorf("ошибка подключения к БД: %w", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ошибка ping БД: %w", err)
	}

	// Миграции
	if err := runMigrations(db); err != nil {
		return nil, fmt.Errorf("ошибка миграций: %w", err)
	}

	return db, nil
}

func runMigrations(db *sql.DB) error {
	// Реализация миграций (в продакшн лучше использовать специализированные инструменты)
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			id BIGINT PRIMARY KEY,
			username TEXT,
			first_name TEXT,
			last_name TEXT,
			created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
			settings JSONB
		);

		CREATE TABLE IF NOT EXISTS tasks (
			id SERIAL PRIMARY KEY,
			user_id BIGINT REFERENCES users(id),
			title TEXT NOT NULL,
			description TEXT,
			start_time TIMESTAMP WITH TIME ZONE,
			end_time TIMESTAMP WITH TIME ZONE,
			priority INTEGER DEFAULT 1,
			category TEXT,
			completed BOOLEAN DEFAULT FALSE,
			created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
		);

		CREATE INDEX IF NOT EXISTS tasks_user_id_idx ON tasks(user_id);
		CREATE INDEX IF NOT EXISTS tasks_start_time_idx ON tasks(start_time);
	`)
	return err
}

// Основная функция
func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Загрузка конфигурации
	config, err := loadConfig("config.json")
	if err != nil {
		log.Fatalf("Ошибка загрузки конфига: %v", err)
	}

	// Инициализация БД
	db, err := initDB(config)
	if err != nil {
		log.Fatalf("Ошибка инициализации БД: %v", err)
	}
	defer db.Close()

	// Инициализация бота
	bot, err := tgbotapi.NewBotAPI(config.BotToken)
	if err != nil {
		log.Panicf("Ошибка инициализации бота: %v", err)
	}
	bot.Debug = config.DebugMode

	log.Printf("Бот авторизован как %s", bot.Self.UserName)

	// Инициализация сервисов
	botService := &BotService{
		bot:      bot,
		db:       db,
		config:   config,
		scheduler: &SchedulerService{db: db},
	}

	notificationService := &NotificationService{
		bot: bot,
		db:  db,
	}

	// Запуск в режиме webhook или long polling
	var updates tgbotapi.UpdatesChannel
	if config.WebhookURL != "" {
		webhookCfg := tgbotapi.NewWebhookWithCert(config.WebhookURL, tgbotapi.FilePath(config.WebhookCert))
		if _, err := bot.SetWebhook(webhookCfg); err != nil {
			log.Panicf("Ошибка установки webhook: %v", err)
		}

		updates = bot.ListenForWebhook("/")
		go func() {
			if err := http.ListenAndServeTLS(fmt.Sprintf(":%d", config.WebhookPort), config.WebhookCert, config.WebhookKey, nil); err != nil {
				log.Panicf("Ошибка запуска webhook сервера: %v", err)
			}
		}()
	} else {
		u := tgbotapi.NewUpdate(0)
		u.Timeout = 60
		updates = bot.GetUpdatesChan(u)
	}

	// Запуск фоновых задач
	g, ctx := errgroup.WithContext(ctx)

	// Фоновые напоминания
	g.Go(func() error {
		return notificationService.runReminders(ctx)
	})

	// Обработка обновлений
	g.Go(func() error {
		for {
			select {
			case <-ctx.Done():
				return nil
			case update := <-updates:
				if update.Message == nil {
					continue
				}

				if err := botService.handleMessage(ctx, update.Message); err != nil {
					log.Printf("Ошибка обработки сообщения: %v", err)
				}
			}
		}
	})

	// Ожидание завершения
	if err := g.Wait(); err != nil {
		log.Printf("Ошибка в работе сервисов: %v", err)
	}
}

// Методы сервисов
func (bs *BotService) handleMessage(ctx context.Context, msg *tgbotapi.Message) error {
	user, err := bs.ensureUserExists(ctx, msg.From)
	if err != nil {
		return fmt.Errorf("ошибка работы с пользователем: %w", err)
	}

	// Определение команды
	switch {
	case msg.IsCommand():
		return bs.handleCommand(ctx, user, msg)
	default:
		return bs.handleTextMessage(ctx, user, msg)
	}
}

func (bs *BotService) ensureUserExists(ctx context.Context, user *tgbotapi.User) (*User, error) {
	var dbUser User
	err := bs.db.QueryRowContext(ctx,
		`INSERT INTO users (id, username, first_name, last_name) 
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE SET
			username = EXCLUDED.username,
			first_name = EXCLUDED.first_name,
			last_name = EXCLUDED.last_name
		RETURNING id, username, first_name, last_name, created_at, settings`,
		user.ID, user.UserName, user.FirstName, user.LastName,
	).Scan(&dbUser.ID, &dbUser.Username, &dbUser.FirstName, &dbUser.LastName, &dbUser.CreatedAt, &dbUser.Settings)

	if err != nil {
		return nil, fmt.Errorf("ошибка создания/обновления пользователя: %w", err)
	}

	return &dbUser, nil
}

func (bs *BotService) handleCommand(ctx context.Context, user *User, msg *tgbotapi.Message) error {
	switch msg.Command() {
	case "start":
		return bs.sendWelcomeMessage(user.ID)
	case "schedule":
		return bs.showDailySchedule(ctx, user.ID, time.Now())
	case "add":
		return bs.startAddTaskFlow(user.ID)
	case "complete":
		return bs.startCompleteTaskFlow(ctx, user.ID)
	case "settings":
		return bs.showSettingsMenu(user.ID)
	case "stats":
		return bs.showUserStatistics(ctx, user.ID)
	default:
		return bs.sendMessage(user.ID, "Неизвестная команда. Используйте /help для списка команд")
	}
}

func (bs *BotService) showDailySchedule(ctx context.Context, userID int64, date time.Time) error {
	rows, err := bs.db.QueryContext(ctx,
		`SELECT id, title, description, start_time, end_time, priority, category, completed
		FROM tasks
		WHERE user_id = $1 AND DATE(start_time) = DATE($2)
		ORDER BY start_time`,
		userID, date,
	)
	if err != nil {
		return fmt.Errorf("ошибка запроса задач: %w", err)
	}
	defer rows.Close()

	var tasks []Task
	for rows.Next() {
		var task Task
		if err := rows.Scan(
			&task.ID, &task.Title, &task.Description,
			&task.StartTime, &task.EndTime, &task.Priority,
			&task.Category, &task.Completed,
		); err != nil {
			return fmt.Errorf("ошибка сканирования задачи: %w", err)
		}
		tasks = append(tasks, task)
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("ошибка итерации задач: %w", err)
	}

	schedule := DaySchedule{
		Date:   date,
		UserID: userID,
		Tasks:  tasks,
	}

	// Рассчет продуктивности
	schedule.ProductivityScore = bs.calculateProductivity(schedule)

	// Форматирование сообщения
	message := formatScheduleMessage(schedule)
	return bs.sendMessage(userID, message)
}

func (bs *BotService) calculateProductivity(schedule DaySchedule) float64 {
	if len(schedule.Tasks) == 0 {
		return 0
	}

	completed := 0
	totalPriority := 0
	for _, task := range schedule.Tasks {
		if task.Completed {
			completed++
			totalPriority += task.Priority
		}
	}

	completionRatio := float64(completed) / float64(len(schedule.Tasks))
	priorityRatio := 1.0
	if totalPriority > 0 {
		priorityRatio = float64(totalPriority) / float64(completed)
	}

	return (completionRatio * 0.7 + priorityRatio * 0.3) * 100
}

func formatScheduleMessage(schedule DaySchedule) string {
	loc, _ := time.LoadLocation("Local") // Используем локальную таймзону
	dateStr := schedule.Date.In(loc).Format("Monday, 02 January 2006")

	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("📅 Расписание на %s\n", dateStr))
	builder.WriteString(fmt.Sprintf("🏆 Продуктивность: %.1f%%\n\n", schedule.ProductivityScore))

	if len(schedule.Tasks) == 0 {
		builder.WriteString("Задачи отсутствуют. Добавьте задачи командой /add")
		return builder.String()
	}

	for _, task := range schedule.Tasks {
		status := "❌"
		if task.Completed {
			status = "✅"
		}

		start := task.StartTime.In(loc).Format("15:04")
		end := task.EndTime.In(loc).Format("15:04")

		builder.WriteString(fmt.Sprintf(
			"%s [%d] %s - %s (%s-%s)\n%s\n\n",
			status, task.ID, task.Title, task.Category, start, end, task.Description,
		))
	}

	return builder.String()
}

func (bs *BotService) sendMessage(chatID int64, text string) error {
	msg := tgbotapi.NewMessage(chatID, text)
	_, err := bs.bot.Send(msg)
	return err
}

// Остальные методы сервисов (startAddTaskFlow, startCompleteTaskFlow, showSettingsMenu, showUserStatistics)
// и методы NotificationService (runReminders) реализуются аналогично с учетом бизнес-логики

// Пример реализации напоминаний
func (ns *NotificationService) runReminders(ctx context.Context) error {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := ns.checkAndSendReminders(ctx); err != nil {
				log.Printf("Ошибка отправки напоминаний: %v", err)
			}
		}
	}
}

func (ns *NotificationService) checkAndSendReminders(ctx context.Context) error {
	now := time.Now()
	rows, err := ns.db.QueryContext(ctx,
		`SELECT t.id, t.user_id, t.title, u.first_name
		FROM tasks t
		JOIN users u ON t.user_id = u.id
		WHERE t.completed = FALSE 
		AND t.start_time BETWEEN $1 AND $2`,
		now.Add(-5*time.Minute), now.Add(5*time.Minute),
	)
	if err != nil {
		return fmt.Errorf("ошибка запроса задач для напоминаний: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var taskID int
		var userID int64
		var title, firstName string

		if err := rows.Scan(&taskID, &userID, &title, &firstName); err != nil {
			return fmt.Errorf("ошибка сканирования задачи: %w", err)
		}

		message := fmt.Sprintf("⏰ %s, время выполнить задачу:\n%s", firstName, title)
		if err := ns.sendReminder(userID, message); err != nil {
			log.Printf("Ошибка отправки напоминания пользователю %d: %v", userID, err)
		}
	}

	return rows.Err()
}

func (ns *NotificationService) sendReminder(chatID int64, text string) error {
	msg := tgbotapi.NewMessage(chatID, text)
	_, err := ns.bot.Send(msg)
	return err
}
