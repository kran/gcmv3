// Package web 层的事件 —— 站点与插件的扩展点。
//
// 机制是 core 的 HookBus（Define 声明签名 / AddHook 注册 / Fire 触发）; 这里只声明
// **web 自己的事件名与签名**。事件在 Open 时就定义好, 所以配置期注册没有时序问题。
//
// 只留真正有消费方的事件: 渲染那套（node_enrich / node_render / candidates）随 F 一起
// 不做 —— 事件名是契约, 空的契约只是负债。
package web

import "github.com/kran/gcmv3/core"

const (
	// HookBeforeMount 挂载前: 插件/站点挂中间件与自己的路由。
	// **必须先于内置路由** —— chi 在注册路由之后再 Use 会 panic, 所以这是中间件与
	// 普通路由的唯一正确时机。签名: func(*Site) error
	HookBeforeMount = "site.before_mount"

	// HookServeFile 服务 /static 与 /uploads 的文件: 插件可以改路径（图片处理、
	// 私有文件鉴权等）。签名: func(*CmsCtx, *string) error
	HookServeFile = "web.serve_file"

	// HookUpload 上传策略 —— 站点/插件声明"谁能传、能传什么、多大、落哪"。
	// 签名: func(*CmsCtx, Upload, *UploadAllow) error
	//
	// 上传不是节点操作（文件先上来、才可能被节点引用）, 所以它是**事件**而不是
	// 按类型的策略。多个 handler 依次收窄 —— 任何 handler 返回错误即拒绝, 且
	// allow 只能**收紧**（谁也不许放宽别人定下的限制）。
	HookUpload = "web.upload"
)

// defineWebHooks 声明 web 事件。签名与事件名都是硬编码的 ⇒ 失败就是编程错误, panic。
func defineWebHooks(engine core.Engine) {
	err := engine.Hooks().Define(map[string]any{
		HookBeforeMount: func(*Site) error { return nil },
		HookServeFile:   func(*CmsCtx, *string) error { return nil },
		HookUpload:      func(*CmsCtx, Upload, *UploadAllow) error { return nil },
	})
	if err != nil {
		panic("web: define hooks: " + err.Error())
	}
}

// Hook 配置期注册 handler（签名不匹配 / 事件名写错 ⇒ 当场 panic, 不静默失效）。
func (s *Site) Hook(name string, fn any) {
	err := s.engine.Hooks().AddHook(name, fn)
	if err != nil {
		panic("web: add hook: " + err.Error())
	}
}
