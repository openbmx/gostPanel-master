package repository

import (
	"errors"

	"gost-panel/internal/model"
	"gost-panel/internal/utils"

	"gorm.io/gorm"
)

// SystemConfigRepository 系统配置仓库
type SystemConfigRepository struct {
	db *gorm.DB
}

// NewSystemConfigRepository 创建系统配置仓库
func NewSystemConfigRepository(db *gorm.DB) *SystemConfigRepository {
	return &SystemConfigRepository{db: db}
}

// Get 获取系统配置 (单例)
func (r *SystemConfigRepository) Get() (*model.SystemConfig, error) {
	var config model.SystemConfig
	// 尝试查找 ID 为 1 的记录
	result := r.db.First(&config, 1)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			// 如果不存在，创建默认配置
			token, err := utils.RandomToken(32)
			if err != nil {
				return nil, err
			}
			config = model.SystemConfig{
				ID:               1,
				SiteTitle:        "Gost Panel",
				LogLevel:         "info",
				LogRetentionDays: 7,
				ObserverToken:    token,
			}
			if err := r.db.Create(&config).Error; err != nil {
				return nil, err
			}
			return &config, nil
		}
		return nil, result.Error
	}

	// 从旧版本升级上来的实例没有上报令牌，这里补齐。
	// 用带条件的 UPDATE 保证并发下只有一个写入者生效，其余读回已写入的值。
	if config.ObserverToken == "" {
		token, err := utils.RandomToken(32)
		if err != nil {
			return nil, err
		}
		res := r.db.Model(&model.SystemConfig{}).
			Where("id = ? AND (observer_token IS NULL OR observer_token = '')", config.ID).
			Update("observer_token", token)
		if res.Error != nil {
			return nil, res.Error
		}
		if res.RowsAffected == 0 {
			// 已被其他并发调用写入，重新读取
			if err := r.db.First(&config, 1).Error; err != nil {
				return nil, err
			}
		} else {
			config.ObserverToken = token
		}
	}

	return &config, nil
}

// Update 更新系统配置
func (r *SystemConfigRepository) Update(config *model.SystemConfig) error {
	// 确保 ID 为 1
	config.ID = 1
	return r.db.Save(config).Error
}
