package commands

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	core "github.com/v2fly/v2ray-core/v5"
	"github.com/v2fly/v2ray-core/v5/common/cmdarg"
	"github.com/v2fly/v2ray-core/v5/common/platform"
	"github.com/v2fly/v2ray-core/v5/main/commands/base"
	"github.com/v2fly/v2ray-core/v5/main/plugins"
)

// CmdRun runs V2Ray with config
var CmdRun = &base.Command{
	CustomFlags: true,
	UsageLine:   "{{.Exec}} run [-c config.json] [-d dir]",
	Short:       "run V2Ray with config",
	Long: `
Run V2Ray with config.

{{.Exec}} will also use the config directory specified by environment 
variable "v2ray.location.confdir". If no config found, it tries 
to load config from one of below:

	1. The default "config.json" in the current directory
	2. The config file from ENV "v2ray.location.config"
	3. The stdin if all failed above

Arguments:

	-c, -config <file>
		Config file for V2Ray. Multiple assign is accepted.

	-d, -confdir <dir>
		A directory with config files. Multiple assign is accepted.

	-r
		Load confdir recursively.

	-format <format>
		Format of config input. (default "auto")

Examples:

	{{.Exec}} {{.LongName}} -c config.json
	{{.Exec}} {{.LongName}} -d path/to/dir

Use "{{.Exec}} help format-loader" for more information about format.
	`,
	Run: executeRun,
}

var (
	configFiles          cmdarg.Arg
	configDirs           cmdarg.Arg
	configFormat         *string
	configDirRecursively *bool
)

// setConfigFlags 注册配置相关的 flags,被 run 与 test 两个子命令共用。
func setConfigFlags(cmd *base.Command) {
	configFormat = cmd.Flag.String("format", core.FormatAuto, "")
	configDirRecursively = cmd.Flag.Bool("r", false, "")

	cmd.Flag.Var(&configFiles, "config", "")
	cmd.Flag.Var(&configFiles, "c", "")
	cmd.Flag.Var(&configDirs, "confdir", "")
	cmd.Flag.Var(&configDirs, "d", "")
}

// executeRun 是 `v2ray run` 子命令的主函数,四个阶段:
//  1. 注册 flags + 预登记插件(插件要在参数解析前注册自己的 flag,如 -pprof);
//  2. 解析参数 → 定位配置文件 → LoadConfig → core.New 装配 Instance(此时未启动);
//  3. server.Start():running=true,级联启动全部 feature —— 整条数据面从此可用;
//  4. 阻塞等 SIGINT/SIGTERM,收到后经 defer server.Close() 级联关闭。
func executeRun(cmd *base.Command, args []string) {
	// 阶段 1a:注册本子命令的 flags。放在函数里而不是 init():
	// base 框架按需执行子命令,flags 属于子命令执行期才注册。
	setConfigFlags(cmd)

	// 阶段 1b:插件预登记。Plugin = func(*base.Command) func() error:
	// 此刻调用它是让插件注册自己的 flag(必须在 Flag.Parse 之前),
	// 返回值是"启动后要长驻运行的函数"(内置唯一插件 pprof:注册 -pprof,
	// 返回 pprof HTTP server 的 ListenAndServe);返回 nil 表示本次未启用。
	var pluginFuncs []func() error
	for _, plugin := range plugins.Plugins {
		if f := plugin(cmd); f != nil {
			pluginFuncs = append(pluginFuncs, f)
		}
	}

	// 阶段 2a:解析命令行参数
	cmd.Flag.Parse(args)
	printVersion()

	// 阶段 2b:定位配置文件,回退链见 getConfigFilePath
	configFiles = getConfigFilePath()

	// 阶段 2c:LoadConfig(JSON→proto Config)→ core.New(查注册表装配 Instance)。
	// 注意 New 只装配、不启动 —— 对应 v2ray.go 的 initInstanceWithConfig。
	server, err := startV2Ray()
	if err != nil {
		base.Fatalf("Failed to start: %s", err)
	}

	// 阶段 3a:插件的长驻函数异步拉起;出错仅记日志,不影响主进程
	for _, f := range pluginFuncs {
		go func(f func() error) {
			if err := f(); err != nil {
				log.Print(err)
			}
		}(f)
	}

	// 阶段 3b:正式启动 —— Instance.Start() 置 running=true 并级联 Start 所有
	// feature,打出 "V2Ray x started";此后 AddFeature 进来的新 feature 会直接热启动
	if err := server.Start(); err != nil {
		base.Fatalf("Failed to start: %s", err)
	}
	// main 返回(收到退出信号)时级联 Close 所有 feature
	defer server.Close()

	// 显式触发 GC:清掉配置加载阶段(protobuf 反射/解析)产生的垃圾,再进入稳态
	runtime.GC()

	// 阶段 4:常驻——阻塞等 SIGINT(Ctrl-C)/SIGTERM;收到后本块返回,
	// 经 defer server.Close() 优雅关闭,进程退出。
	// 注意:没有"第二次信号强制退出"的保护,若 Close 卡住进程将无法退出。
	{
		osSignals := make(chan os.Signal, 1)
		signal.Notify(osSignals, os.Interrupt, syscall.SIGTERM)
		<-osSignals
	}
}

func fileExists(file string) bool {
	info, err := os.Stat(file)
	return err == nil && !info.IsDir()
}

func dirExists(file string) bool {
	if file == "" {
		return false
	}
	info, err := os.Stat(file)
	return err == nil && info.IsDir()
}

func readConfDir(dirPath string, extension []string) cmdarg.Arg {
	confs, err := os.ReadDir(dirPath)
	if err != nil {
		base.Fatalf("failed to read dir %s: %s", dirPath, err)
	}
	files := make(cmdarg.Arg, 0)
	for _, f := range confs {
		ext := filepath.Ext(f.Name())
		for _, e := range extension {
			if strings.EqualFold(e, ext) {
				files.Set(filepath.Join(dirPath, f.Name()))
				break
			}
		}
	}
	return files
}

// getFolderFiles get files in the folder and it's children
func readConfDirRecursively(dirPath string, extension []string) cmdarg.Arg {
	files := make(cmdarg.Arg, 0)
	err := filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		ext := filepath.Ext(path)
		for _, e := range extension {
			if strings.EqualFold(e, ext) {
				files.Set(path)
				break
			}
		}
		return nil
	})
	if err != nil {
		base.Fatalf("failed to read dir %s: %s", dirPath, err)
	}
	return files
}

// getConfigFilePath 定位要加载的配置文件,优先级从高到低:
//  1. -c/-config 显式给出的文件(解析参数后已在 configFiles 里,非空直接返回);
//  2. -d/-confdir 指定的目录(按 format 对应的扩展名扫描,-r 时递归);
//  3. 环境变量 v2ray.location.confdir 指向的目录;
//  4. 当前工作目录下的 config.json;
//  5. 环境变量 v2ray.location.config 指向的文件。
//
// 全部落空则返回 nil —— 后续 LoadConfig 会退化为从 stdin 读配置。
func getConfigFilePath() cmdarg.Arg {
	extension, err := core.GetLoaderExtensions(*configFormat)
	if err != nil {
		base.Fatalf("%v", err.Error())
	}
	dirReader := readConfDir
	if *configDirRecursively {
		dirReader = readConfDirRecursively
	}
	if len(configDirs) > 0 {
		for _, d := range configDirs {
			log.Println("Using confdir from arg:", d)
			configFiles = append(configFiles, dirReader(d, extension)...)
		}
	} else if envConfDir := platform.GetConfDirPath(); dirExists(envConfDir) {
		log.Println("Using confdir from env:", envConfDir)
		configFiles = append(configFiles, dirReader(envConfDir, extension)...)
	}
	if len(configFiles) > 0 {
		return configFiles
	}

	if len(configFiles) == 0 && len(configDirs) > 0 {
		base.Fatalf("no config file found with extension: %s", extension)
	}

	if workingDir, err := os.Getwd(); err == nil {
		configFile := filepath.Join(workingDir, "config.json")
		if fileExists(configFile) {
			log.Println("Using default config: ", configFile)
			return cmdarg.Arg{configFile}
		}
	}

	if configFile := platform.GetConfigurationPath(); fileExists(configFile) {
		log.Println("Using config from env: ", configFile)
		return cmdarg.Arg{configFile}
	}

	return nil
}

// startV2Ray 加载配置并装配 Instance:LoadConfig(格式解析与多文件合并)
// → core.New(查注册表装配 feature、补兜底 feature,见 v2ray.go)。
// 返回的 server 尚未 Start,启动时机由 executeRun 决定(先拉插件,再 Start)。
func startV2Ray() (core.Server, error) {
	config, err := core.LoadConfig(*configFormat, configFiles)
	if err != nil {
		if len(configFiles) == 0 {
			err = newError("failed to load config").Base(err)
		} else {
			err = newError(fmt.Sprintf("failed to load config: %s", configFiles)).Base(err)
		}
		return nil, err
	}

	server, err := core.New(config)
	if err != nil {
		return nil, newError("failed to create server").Base(err)
	}

	return server, nil
}
