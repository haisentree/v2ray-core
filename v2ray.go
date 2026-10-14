package core

import (
	"context"
	"reflect"
	sync "sync"

	"github.com/v2fly/v2ray-core/v5/common/environment/deferredpersistentstorage"
	"github.com/v2fly/v2ray-core/v5/common/environment/filesystemimpl"
	"github.com/v2fly/v2ray-core/v5/features/extension/storage"

	"github.com/v2fly/v2ray-core/v5/common"
	"github.com/v2fly/v2ray-core/v5/common/environment"
	"github.com/v2fly/v2ray-core/v5/common/environment/systemnetworkimpl"
	"github.com/v2fly/v2ray-core/v5/common/environment/transientstorageimpl"
	"github.com/v2fly/v2ray-core/v5/common/serial"
	"github.com/v2fly/v2ray-core/v5/features"
	"github.com/v2fly/v2ray-core/v5/features/dns"
	"github.com/v2fly/v2ray-core/v5/features/dns/localdns"
	"github.com/v2fly/v2ray-core/v5/features/inbound"
	"github.com/v2fly/v2ray-core/v5/features/outbound"
	"github.com/v2fly/v2ray-core/v5/features/policy"
	"github.com/v2fly/v2ray-core/v5/features/routing"
	"github.com/v2fly/v2ray-core/v5/features/stats"
)

// Server is an instance of V2Ray. At any time, there must be at most one Server instance running.
type Server interface {
	common.Runnable
}

// ServerType returns the type of the server.
func ServerType() interface{} {
	return (*Instance)(nil)
}

type resolution struct {
	deps     []reflect.Type
	callback interface{}
}

func getFeature(allFeatures []features.Feature, t reflect.Type) features.Feature {
	for _, f := range allFeatures {
		if reflect.TypeOf(f.Type()) == t {
			return f
		}
	}
	return nil
}

func (r *resolution) resolve(allFeatures []features.Feature) (bool, error) {
	var fs []features.Feature
	for _, d := range r.deps {
		f := getFeature(allFeatures, d)
		if f == nil {
			return false, nil
		}
		fs = append(fs, f)
	}

	callback := reflect.ValueOf(r.callback)
	var input []reflect.Value
	callbackType := callback.Type()
	for i := 0; i < callbackType.NumIn(); i++ {
		pt := callbackType.In(i)
		for _, f := range fs {
			if reflect.TypeOf(f).AssignableTo(pt) {
				input = append(input, reflect.ValueOf(f))
				break
			}
		}
	}

	if len(input) != callbackType.NumIn() {
		panic("Can't get all input parameters")
	}

	var err error
	ret := callback.Call(input)
	errInterface := reflect.TypeOf((*error)(nil)).Elem()
	for i := len(ret) - 1; i >= 0; i-- {
		if ret[i].Type() == errInterface {
			v := ret[i].Interface()
			if v != nil {
				err = v.(error)
			}
			break
		}
	}

	return true, err
}

// Instance combines all functionalities in V2Ray.
//
// 阅读备注(概念模型):Instance 是运行时管理者,本身不做任何具体功能,
// 只持有一个 Feature 数组并负责三件事:装配(AddFeature)、依赖解析(RequireFeatures)、
// 生命周期(Start/Close 级联)。Feature 才是功能的实现——dispatcher、路由、DNS、
// 出入站管理器……全部抽象模型都是下面数组里的一项。
//
// 每个 Feature 只需实现三个方法(见 features/feature.go):
//
//	Type() interface{} // 返回"接口类型的 nil 指针"作查找键,如 (*Dispatcher)(nil)
//	Start() error      // Instance.Start 级联调用
//	Close() error      // Instance.Close 级联调用
//
// 组件互取依赖统一走 GetFeature(Type())(本文件 :361)。
type Instance struct {
	access sync.Mutex
	// features 即整个运行时:一个 v2ray 进程 = 这堆 Feature 的集合。
	features []features.Feature
	// 依赖未凑齐而挂起的回调;每次 AddFeature 后重试 resolve(见 RequireFeatures)。
	featureResolutions []resolution
	// Start 后为 true;此后 AddFeature 进来的新 feature 会直接热启动。
	running bool
	env     environment.RootEnvironment

	ctx context.Context
}

func AddInboundHandler(server *Instance, config *InboundHandlerConfig) error {
	inboundManager := server.GetFeature(inbound.ManagerType()).(inbound.Manager)
	proxyEnv := server.env.ProxyEnvironment("i" + config.Tag)
	rawHandler, err := CreateObjectWithEnvironment(server, config, proxyEnv)
	if err != nil {
		return err
	}
	handler, ok := rawHandler.(inbound.Handler)
	if !ok {
		return newError("not an InboundHandler")
	}
	if err := inboundManager.AddHandler(server.ctx, handler); err != nil {
		return err
	}
	return nil
}

func addInboundHandlers(server *Instance, configs []*InboundHandlerConfig) error {
	for _, inboundConfig := range configs {
		if err := AddInboundHandler(server, inboundConfig); err != nil {
			return err
		}
	}

	return nil
}

func AddOutboundHandler(server *Instance, config *OutboundHandlerConfig) error {
	outboundManager := server.GetFeature(outbound.ManagerType()).(outbound.Manager)
	proxyEnv := server.env.ProxyEnvironment("o" + config.Tag)
	rawHandler, err := CreateObjectWithEnvironment(server, config, proxyEnv)
	if err != nil {
		return err
	}
	handler, ok := rawHandler.(outbound.Handler)
	if !ok {
		return newError("not an OutboundHandler")
	}
	if err := outboundManager.AddHandler(server.ctx, handler); err != nil {
		return err
	}
	return nil
}

func RemoveOutboundHandler(server *Instance, tag string) error {
	outboundManager := server.GetFeature(outbound.ManagerType()).(outbound.Manager)
	if err := outboundManager.RemoveHandler(server.ctx, tag); err != nil {
		return err
	}

	if err := server.env.DropProxyEnvironment("o" + tag); err != nil {
		return err
	}
	return nil
}

func addOutboundHandlers(server *Instance, configs []*OutboundHandlerConfig) error {
	for _, outboundConfig := range configs {
		if err := AddOutboundHandler(server, outboundConfig); err != nil {
			return err
		}
	}

	return nil
}

// RequireFeatures is a helper function to require features from Instance in context.
// See Instance.RequireFeatures for more information.
func RequireFeatures(ctx context.Context, callback interface{}) error {
	v := MustFromContext(ctx)
	return v.RequireFeatures(callback)
}

// New returns a new V2Ray instance based on given configuration.
// The instance is not started at this point.
// To ensure V2Ray instance works properly, the config must contain one Dispatcher, one InboundHandlerManager and one OutboundHandlerManager. Other features are optional.
func New(config *Config) (*Instance, error) {
	server := &Instance{ctx: context.Background()}

	done, err := initInstanceWithConfig(config, server)
	if done {
		return nil, err
	}

	return server, nil
}

func NewWithContext(ctx context.Context, config *Config) (*Instance, error) {
	server := &Instance{ctx: ctx}

	done, err := initInstanceWithConfig(config, server)
	if done {
		return nil, err
	}

	return server, nil
}

func initInstanceWithConfig(config *Config, server *Instance) (bool, error) {
	if config.Transport != nil {
		features.PrintDeprecatedFeatureWarning("global transport settings")
	}
	if err := config.Transport.Apply(); err != nil {
		return true, err
	}

	defaultNetworkImpl := systemnetworkimpl.NewSystemNetworkDefault()
	defaultFilesystemImpl := filesystemimpl.NewDefaultFileSystemDefaultImpl()
	deferredPersistentStorageImpl := deferredpersistentstorage.NewDeferredPersistentStorage(server.ctx)
	server.env = environment.NewRootEnvImpl(server.ctx,
		transientstorageimpl.NewScopedTransientStorageImpl(),
		defaultNetworkImpl.Dialer(),
		defaultNetworkImpl.Listener(),
		defaultFilesystemImpl,
		deferredPersistentStorageImpl)

	for _, appSettings := range config.App {
		settings, err := serial.GetInstanceOf(appSettings)
		if err != nil {
			return true, err
		}
		key := appSettings.TypeUrl
		appEnv := server.env.AppEnvironment(key)
		obj, err := CreateObjectWithEnvironment(server, settings, appEnv)
		if err != nil {
			return true, err
		}
		if feature, ok := obj.(features.Feature); ok {
			if err := server.AddFeature(feature); err != nil {
				return true, err
			}
		}
	}

	essentialFeatures := []struct {
		Type     interface{}
		Instance features.Feature
	}{
		{dns.ClientType(), localdns.New()},
		{policy.ManagerType(), policy.DefaultManager{}},
		{routing.RouterType(), routing.DefaultRouter{}},
		{stats.ManagerType(), stats.NoopManager{}},
	}

	for _, f := range essentialFeatures {
		if server.GetFeature(f.Type) == nil {
			if err := server.AddFeature(f.Instance); err != nil {
				return true, err
			}
		}
	}

	if server.featureResolutions != nil {
		return true, newError("not all dependency are resolved.")
	}

	if persistentStorageService := server.GetFeature(storage.ScopedPersistentStorageServiceType); persistentStorageService != nil {
		deferredPersistentStorageImpl.ProvideInner(server.ctx, persistentStorageService.(storage.ScopedPersistentStorage))
	} else {
		deferredPersistentStorageImpl.ProvideInner(server.ctx, nil)
	}

	if err := addInboundHandlers(server, config.Inbound); err != nil {
		return true, err
	}

	if err := addOutboundHandlers(server, config.Outbound); err != nil {
		return true, err
	}
	return false, nil
}

// Type implements common.HasType.
func (s *Instance) Type() interface{} {
	return ServerType()
}

// Close shutdown the V2Ray instance.
func (s *Instance) Close() error {
	s.access.Lock()
	defer s.access.Unlock()

	s.running = false

	var errors []interface{}
	for _, f := range s.features {
		if err := f.Close(); err != nil {
			errors = append(errors, err)
		}
	}
	if len(errors) > 0 {
		return newError("failed to close all features").Base(newError(serial.Concat(errors...)))
	}

	return nil
}

// RequireFeatures registers a callback, which will be called when all dependent features are registered.
// The callback must be a func(). All its parameters must be features.Feature.
func (s *Instance) RequireFeatures(callback interface{}) error {
	callbackType := reflect.TypeOf(callback)
	if callbackType.Kind() != reflect.Func {
		panic("not a function")
	}

	var featureTypes []reflect.Type
	for i := 0; i < callbackType.NumIn(); i++ {
		featureTypes = append(featureTypes, reflect.PtrTo(callbackType.In(i)))
	}

	r := resolution{
		deps:     featureTypes,
		callback: callback,
	}
	if finished, err := r.resolve(s.features); finished {
		return err
	}
	s.featureResolutions = append(s.featureResolutions, r)
	return nil
}

// AddFeature registers a feature into current Instance.
func (s *Instance) AddFeature(feature features.Feature) error {
	s.features = append(s.features, feature)

	if s.running {
		if err := feature.Start(); err != nil {
			newError("failed to start feature").Base(err).WriteToLog()
		}
		return nil
	}

	if s.featureResolutions == nil {
		return nil
	}

	var pendingResolutions []resolution
	for _, r := range s.featureResolutions {
		finished, err := r.resolve(s.features)
		if finished && err != nil {
			return err
		}
		if !finished {
			pendingResolutions = append(pendingResolutions, r)
		}
	}
	if len(pendingResolutions) == 0 {
		s.featureResolutions = nil
	} else if len(pendingResolutions) < len(s.featureResolutions) {
		s.featureResolutions = pendingResolutions
	}

	return nil
}

// GetFeature returns a feature of the given type, or nil if such feature is not registered.
func (s *Instance) GetFeature(featureType interface{}) features.Feature {
	return getFeature(s.features, reflect.TypeOf(featureType))
}

// Start starts the V2Ray instance, including all registered features. When Start returns error, the state of the instance is unknown.
// A V2Ray instance can be started only once. Upon closing, the instance is not guaranteed to start again.
//
// v2ray:api:stable
func (s *Instance) Start() error {
	s.access.Lock()
	defer s.access.Unlock()

	s.running = true
	for _, f := range s.features {
		if err := f.Start(); err != nil {
			return err
		}
	}

	newError("V2Ray ", Version(), " started").AtWarning().WriteToLog()

	return nil
}
