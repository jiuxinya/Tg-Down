package desktop

// Autostart 是开机自启能力的平台抽象。
// 实现约定：Enable/Disable 幂等，重复启用不报错；Enabled 仅报告是否已配置。
type Autostart interface {
	Enabled() (bool, error)
	Enable() error
	Disable() error
}
