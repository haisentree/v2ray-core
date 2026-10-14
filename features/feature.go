package features

import "github.com/v2fly/v2ray-core/v5/common"

//go:generate go run github.com/v2fly/v2ray-core/v5/common/errors/errorgen

// Feature is the interface for V2Ray features. All features must implement this interface.
// All existing features have an implementation in app directory. These features can be replaced by third-party ones.
//
// 阅读备注:Feature 是 v2ray 组件的最小契约,只需实现三个方法——
//
//	Type() interface{} // 来自 common.HasType:返回"接口类型的 nil 指针"作注册/查找键
//	Start() error      // 来自 common.Runnable
//	Close() error      // 来自 common.Runnable
//
// 满足这三方法即可挂进 core.Instance 的 features 数组;具体功能(dispatcher、
// 路由、DNS、出入站管理器……)都在各 Feature 实现里,Instance 只做管理者。
type Feature interface {
	common.HasType
	common.Runnable
}

// PrintDeprecatedFeatureWarning prints a warning for deprecated feature.
func PrintDeprecatedFeatureWarning(feature string) {
	newError("You are using a deprecated feature: " + feature + ". Please update your config file with latest configuration format, or update your client software.").WriteToLog()
}

// TaggedFeatures unstable
type TaggedFeatures interface {
	GetFeaturesByTag(tag string) (Feature, error)
	common.Runnable
}
