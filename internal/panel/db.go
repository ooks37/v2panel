package panel

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// DB 全局数据库句柄 (与 work-v 一致的单例风格)。
var DB *gorm.DB

// InitDB 打开 SQLite 并完成迁移与初始化。
//
// 驱动选 glebarez/sqlite (纯 Go 包装 modernc.org/sqlite): CGO_ENABLED=0 也能用,
// 与 82 上「只有 812MB 内存、必须静态单二进制」的约束一致。
func InitDB(dbPath string) (*gorm.DB, error) {
	if dbPath == "" {
		dbPath = "data/v2panel.db"
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("创建数据库目录失败: %w", err)
	}

	// WAL + busy_timeout: 心跳与流量上报都是高频写, 不设这两个必然 database is locked。
	dsn := fmt.Sprintf("%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)", dbPath)
	logCfg := &gorm.Config{
		Logger: logger.New(log.New(os.Stderr, "\r\n", log.LstdFlags), logger.Config{
			SlowThreshold:             500 * time.Millisecond,
			LogLevel:                  logger.Warn,
			IgnoreRecordNotFoundError: true,
			Colorful:                  false,
		}),
	}
	db, err := gorm.Open(sqlite.Open(dsn), logCfg)
	if err != nil {
		return nil, fmt.Errorf("连接 SQLite 失败: %w", err)
	}
	if sqlDB, err := db.DB(); err == nil {
		// 单写句柄: SQLite 同一时刻只允许一个写者, 多连接只会把写锁冲突变成重试风暴。
		sqlDB.SetMaxOpenConns(1)
		sqlDB.SetMaxIdleConns(1)
	}

	if err := db.AutoMigrate(
		&Admin{}, &Node{}, &Inbound{}, &User{}, &TrafficSample{}, &SystemSetting{},
		&Plan{}, &UserTrafficSample{}, &Announcement{}, &Order{},
	); err != nil {
		return nil, fmt.Errorf("数据库表迁移失败: %w", err)
	}
	DB = db

	initSystemSecrets(db)
	initSafeEntry(db)
	initDefaultAdmin(db)

	// 历史无密码用户补齐账号口令 (与 work-v 相同的历史数据修复)
	db.Model(&User{}).Where("password = '' OR password IS NULL").
		Updates(map[string]any{"password": gorm.Expr("substr(uuid, 1, 8)")})

	// 历史用户补齐套餐相关字段的默认值。
	// AutoMigrate 加列时会给已有行填零值, 但 reset_mode='' 在业务上无意义
	// (代码里按 monthly 处理), 显式写成 monthly 免得日志/导出里出现空字符串。
	db.Model(&User{}).Where("reset_mode = '' OR reset_mode IS NULL").
		Updates(map[string]any{"reset_mode": "monthly"})

	return db, nil
}

// GetSetting 读系统设置。
func GetSetting(key, defaultVal string) string {
	if DB == nil {
		return defaultVal
	}
	var s SystemSetting
	if err := DB.Where("key = ?", key).First(&s).Error; err == nil && s.Value != "" {
		return s.Value
	}
	return defaultVal
}

// SetSetting 原子 upsert 系统设置。
//
// updated_at 必须显式写 time.Now(): 历史实现误把时区字符串塞进该列, 之后该行再也无法被
// GORM 扫进 time.Time, 读取静默回落默认值 —— 表现为「域名/密钥改第二次就丢」。
func SetSetting(key, val string) error {
	if DB == nil {
		return fmt.Errorf("数据库未初始化")
	}
	now := time.Now()
	return DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.Assignments(map[string]any{"value": val, "updated_at": now}),
	}).Create(&SystemSetting{Key: key, Value: val, UpdatedAt: now}).Error
}

// GetSafeEntryConfig 读安全入口配置。
// 关闭状态用显式开关 safe_entry_enabled="0" 表达, 不能靠「路径为空」——
// 空值无法与「从未配置」区分, 重启后会被默认值重新打开。
func GetSafeEntryConfig() (enabled bool, path string) {
	if DB != nil {
		var s SystemSetting
		if err := DB.Where("key = ?", "safe_path").First(&s).Error; err == nil {
			path = strings.TrimSpace(s.Value)
		}
		if GetSetting("safe_entry_enabled", "1") == "0" {
			return false, path
		}
	}
	if path == "" || path == "/" {
		path = DefaultSafePath
	}
	return true, path
}

func randHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}

func initSystemSecrets(db *gorm.DB) {
	for _, key := range []string{"jwt_secret", "entrance_salt"} {
		var s SystemSetting
		if err := db.Where("key = ?", key).First(&s).Error; err == nil && s.Value != "" {
			continue
		}
		val := randHex(32)
		if val == "" {
			log.Printf("[v2panel] 生成 %s 失败: 随机源不可用", key)
			continue
		}
		db.Save(&SystemSetting{Key: key, Value: val, UpdatedAt: time.Now()})
	}
}

// initSafeEntry 宝塔风格安全入口: 首次启动生成随机路径。
// 固定默认值写在公开文档里等于没有防护, 故随机化, 地址用 `v2panel entry` 查询。
func initSafeEntry(db *gorm.DB) {
	var s SystemSetting
	if err := db.Where("key = ?", "safe_path").First(&s).Error; err == nil && strings.TrimSpace(s.Value) != "" {
		return
	}
	p := randHex(8)
	if p == "" {
		return
	}
	newPath := "/" + p
	if err := db.Save(&SystemSetting{Key: "safe_path", Value: newPath, UpdatedAt: time.Now()}).Error; err != nil {
		log.Printf("[v2panel] 保存安全入口失败: %v", err)
		return
	}
	log.Printf("[v2panel 安全防护] 已生成随机安全入口: %s (用 v2panel entry 查看完整地址)", newPath)
}

func initDefaultAdmin(db *gorm.DB) {
	var count int64
	db.Model(&Admin{}).Count(&count)
	if count > 0 {
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(DefaultAdminPass), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("初始化密码哈希失败: %v", err)
	}
	if err := db.Create(&Admin{Username: DefaultAdminUser, PasswordHash: string(hash)}).Error; err != nil {
		log.Printf("[v2panel] 创建默认管理员失败: %v", err)
		return
	}
	log.Println("==================================================")
	log.Printf("[v2panel] 已初始化默认管理员: %s / %s (请尽快修改)", DefaultAdminUser, DefaultAdminPass)
	log.Println("==================================================")
}

// EnsureDefaultUser 保证至少存在一个订阅账号, 返回它。
// 订阅链接必须有账号载体 (UUID 是客户端鉴权凭据), 空库时自动建一个默认账号。
func EnsureDefaultUser() (*User, error) {
	var u User
	if err := DB.Order("id asc").First(&u).Error; err == nil {
		return &u, nil
	}
	u = User{
		Username: "default",
		UUID:     newUUID(),
		SubToken: randHex(16),
		Status:   "active",
	}
	u.Password = strings.ReplaceAll(u.UUID, "-", "")
	if len(u.Password) > 8 {
		u.Password = u.Password[:8]
	}
	if err := DB.Create(&u).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

// newUUID 生成 RFC4122 v4 UUID (避免为此再引入一个依赖)。
func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// 退化为时间派生值, 保证不返回空串
		return fmt.Sprintf("%08x-0000-4000-8000-%012x", time.Now().UnixNano()&0xffffffff, time.Now().UnixNano()&0xffffffffffff)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// NewToken 生成 agent 连接凭证 (32 位十六进制, 无连字符便于命令行传递)。
func NewToken() string { return randHex(16) }

// RandomToken 生成指定长度的随机十六进制串 (供订阅 token 等使用)。
func RandomToken(n int) string { return randHex(n) }
