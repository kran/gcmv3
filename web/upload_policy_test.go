package web

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kran/gcmv3/core"
)

// uploadTree 列出 uploads/ 下的所有文件（相对路径）—— 用来断言"被拒的上传没落盘"。
func uploadTree(t *testing.T, site *Site) []string {
	t.Helper()
	root := filepath.Join(site.basedir, uploadsDir)
	var out []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return out
}

// 规则按身份拒: 资料没审核过的会员传不了（登录过了也不行）。
func TestUploadRuleDeniesByIdentity(t *testing.T) {
	site := newPolicySite(t)
	site.Hook(HookUpload, func(c *CmsCtx, _ Upload, allow *UploadAllow) error {
		node, err := c.Principal()
		if err != nil {
			return Unauthorized("请先登录")
		}
		if node.Fields.Str("approval_state") != "approved" {
			return Forbidden("资料审核通过后才能上传")
		}
		allow.Extensions("png")
		return nil
	})
	site.Setup()
	token := memberToken(t, site)

	got := uploadFile(t, site, token, "a.png", sniffSamples[".png"], nil)
	if got.Code != http.StatusForbidden || !strings.Contains(got.Body.String(), "审核") {
		t.Fatalf("未审核该拒: %d %q", got.Code, got.Body.String())
	}
	if files := uploadTree(t, site); len(files) != 0 {
		t.Fatalf("被拒的上传不该落盘: %v", files)
	}
	// 审核通过后就能传（规则里读的是库里的手续状态, 每请求都重新判）
	err := site.Engine().PatchNode(nil, cmsNodeID(t, site, token), &core.NodePatch{
		Revision: ptrInt64(1), Fields: core.Fields{"approval_state": "approved"}})
	if err != nil {
		t.Fatal(err)
	}
	got = uploadFile(t, site, token, "a.png", sniffSamples[".png"], nil)
	if got.Code != http.StatusOK {
		t.Fatalf("审核后该能传: %d %q", got.Code, got.Body.String())
	}
}

// 规则收窄扩展名: 只列了 png 就传不了 pdf（哪怕框架白名单里有）。
func TestUploadRuleNarrowsExtensions(t *testing.T) {
	site := newPolicySite(t)
	site.Hook(HookUpload, func(_ *CmsCtx, _ Upload, allow *UploadAllow) error {
		allow.Extensions("PNG", ".webp") // 大写与不带点都该被归一化
		return nil
	})
	site.Setup()
	token := memberToken(t, site)

	if got := uploadFile(t, site, token, "a.png", sniffSamples[".png"], nil); got.Code != http.StatusOK {
		t.Fatalf("png 该允许: %d %q", got.Code, got.Body.String())
	}
	if got := uploadFile(t, site, token, "a.webp", sniffSamples[".webp"], nil); got.Code != http.StatusOK {
		t.Fatalf("webp 该允许: %d %q", got.Code, got.Body.String())
	}
	got := uploadFile(t, site, token, "a.pdf", sniffSamples[".pdf"], nil)
	if got.Code != http.StatusUnprocessableEntity || !strings.Contains(got.Body.String(), "不允许") {
		t.Fatalf("pdf 该被规则拦下: %d %q", got.Code, got.Body.String())
	}
	// 白名单外的类型仍然是框架先拦（消息不同）
	got = uploadFile(t, site, token, "a.svg", []byte("<svg/>"), nil)
	if got.Code != http.StatusUnprocessableEntity || !strings.Contains(got.Body.String(), "不支持") {
		t.Fatalf("svg 该被白名单拦下: %d %q", got.Code, got.Body.String())
	}
}

// 规则的 MaxBytes: 超了 413 且**磁盘上不留文件**; 正好等于上限则通过。
func TestUploadRuleMaxBytes(t *testing.T) {
	site := newPolicySite(t)
	site.Hook(HookUpload, func(_ *CmsCtx, _ Upload, allow *UploadAllow) error {
		allow.Extensions("png")
		allow.MaxBytes(2048)
		return nil
	})
	site.Setup()
	token := memberToken(t, site)

	// 正好 2048 字节（含 png 头）⇒ 通过
	exact := append(bytes.Repeat(sniffSamples[".png"], 2048/16), make([]byte, 2048-len(sniffSamples[".png"])*(2048/16))...)
	got := uploadFile(t, site, token, "exact.png", exact, nil)
	if got.Code != http.StatusOK {
		t.Fatalf("正好上限该通过: %d %q (%d 字节)", got.Code, got.Body.String(), len(exact))
	}
	// 多一个字节 ⇒ 413
	got = uploadFile(t, site, token, "over.png", append(exact, 'x'), nil)
	if got.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超一个字节该 413: %d %q", got.Code, got.Body.String())
	}
	// 只有"正好"那一个文件落盘
	files := uploadTree(t, site)
	if len(files) != 1 {
		t.Fatalf("只该留一个文件: %v", files)
	}
}

// 规则的 Dir: 路径里带上归属（回收/统计靠它）, 且仍能被服务。
func TestUploadRuleDir(t *testing.T) {
	site := newPolicySite(t)
	site.Hook(HookUpload, func(_ *CmsCtx, _ Upload, allow *UploadAllow) error {
		allow.Extensions("png")
		allow.Dir("member/123")
		return nil
	})
	site.Setup()
	token := memberToken(t, site)

	got := uploadFile(t, site, token, "a.png", sniffSamples[".png"], nil)
	if got.Code != http.StatusOK {
		t.Fatalf("上传 = %d %q", got.Code, got.Body.String())
	}
	uploaded := uploadPath(t, got.Body.String())
	if !strings.HasPrefix(uploaded, "/uploads/member/123/") {
		t.Fatalf("路径该在归属目录下: %q", uploaded)
	}
	if !strings.HasSuffix(uploaded, ".png") {
		t.Fatalf("路径该以扩展名结尾: %q", uploaded)
	}
	if served := do(t, site, http.MethodGet, uploaded); served.Code != http.StatusOK {
		t.Fatalf("该能服务到: %d", served.Code)
	}
	if files := uploadTree(t, site); len(files) != 1 || !strings.HasPrefix(files[0], "member/123/") {
		t.Fatalf("落盘位置不对: %v", files)
	}
}

// 规则自己的错误（配置错）⇒ 500 且不落盘: 声明白名单外的扩展名、目录越界。
func TestUploadRuleConfigErrors(t *testing.T) {
	cases := map[string]func(*CmsCtx, Upload, *UploadAllow) error{
		"扩展名不在白名单": func(_ *CmsCtx, _ Upload, allow *UploadAllow) error {
			allow.Extensions("svg")
			return nil
		},
		"目录越界": func(_ *CmsCtx, _ Upload, allow *UploadAllow) error {
			allow.Extensions("png")
			allow.Dir("../../etc")
			return nil
		},
		"目录是绝对路径": func(_ *CmsCtx, _ Upload, allow *UploadAllow) error {
			allow.Extensions("png")
			allow.Dir("/etc")
			return nil
		},
	}
	for name, rule := range cases {
		t.Run(name, func(t *testing.T) {
			site := newPolicySite(t)
			site.Hook(HookUpload, rule)
			site.Setup()
			token := memberToken(t, site)
			got := uploadFile(t, site, token, "a.png", sniffSamples[".png"], nil)
			if got.Code != http.StatusInternalServerError {
				t.Fatalf("配置错该 500: %d %q", got.Code, got.Body.String())
			}
			if files := uploadTree(t, site); len(files) != 0 {
				t.Fatalf("配置错时不该落盘: %v", files)
			}
		})
	}
}

// **登录是框架底线**: 规则不判身份也传不了（fail-closed, 不是 fail-open）。
func TestUploadRequiresLoginEvenWithRule(t *testing.T) {
	site := newPolicySite(t)
	site.Hook(HookUpload, func(_ *CmsCtx, _ Upload, allow *UploadAllow) error {
		allow.Extensions("png") // 规则什么都没判
		return nil
	})
	site.Setup()
	got := uploadFile(t, site, "", "a.png", sniffSamples[".png"], nil)
	if got.Code != http.StatusUnauthorized {
		t.Fatalf("匿名该 401: %d %q", got.Code, got.Body.String())
	}
	if files := uploadTree(t, site); len(files) != 0 {
		t.Fatalf("匿名不该落盘: %v", files)
	}
}

// hook 注册的两种错误 ⇒ panic（签名不匹配 / 事件名不存在）—— 上传策略也是事件,
// 所以走的是同一套注册期校验。
func TestUploadPolicyRegistration(t *testing.T) {
	cases := map[string]func(*Site){
		"签名不匹配": func(s *Site) {
			s.Hook(HookUpload, func(*CmsCtx) error { return nil })
		},
		"事件名不存在": func(s *Site) {
			s.Hook("web.nope", func(*CmsCtx, Upload, *UploadAllow) error { return nil })
		},
	}
	for name, apply := range cases {
		t.Run(name, func(t *testing.T) {
			site := newPolicySite(t)
			defer func() {
				if recover() == nil {
					t.Fatal("该 panic")
				}
			}()
			apply(site)
		})
	}
}

// **只能收窄**: 两个 handler 各说各的 ⇒ 取交集, 谁也不许放宽别人。
func TestUploadPolicyOnlyNarrows(t *testing.T) {
	site := newPolicySite(t)
	// 站点: 只允许图片 + 2 MiB
	site.Hook(HookUpload, func(_ *CmsCtx, _ Upload, allow *UploadAllow) error {
		allow.Extensions("png", "jpg", "webp")
		allow.MaxBytes(2 << 20)
		return nil
	})
	// 插件: 想放宽（多加了 pdf、把上限抬到 10 MiB）—— 都不能生效
	site.Hook(HookUpload, func(_ *CmsCtx, _ Upload, allow *UploadAllow) error {
		allow.Extensions("png", "pdf") // png 交集里还在; pdf 是站点没允许的, 加不进来
		allow.MaxBytes(10 << 20)
		return nil
	})
	site.Setup()
	token := memberToken(t, site)

	// 插件"加"的 pdf 进不来
	got := uploadFile(t, site, token, "a.pdf", sniffSamples[".pdf"], nil)
	if got.Code != http.StatusUnprocessableEntity {
		t.Fatalf("放宽的扩展名该无效: %d %q", got.Code, got.Body.String())
	}
	// 站点允许的 png 还在（交集非空, 所以不是"配置错"）
	got = uploadFile(t, site, token, "a.png", sniffSamples[".png"], nil)
	if got.Code != http.StatusOK {
		t.Fatalf("png 该允许: %d %q", got.Code, got.Body.String())
	}
	// 站点没允许的 webp 也不在（插件只提了 png+pdf ⇒ 交集 = {png}）
	got = uploadFile(t, site, token, "a.webp", sniffSamples[".webp"], nil)
	if got.Code != http.StatusUnprocessableEntity {
		t.Fatalf("插件没提的 webp 该不在交集里: %d %q", got.Code, got.Body.String())
	}
}

// MaxBytes 也只能降: 站点定了 1 KiB, 插件想抬到 4 KiB ⇒ 仍然 1 KiB。
func TestUploadPolicyMaxBytesOnlyNarrows(t *testing.T) {
	site := newPolicySite(t)
	site.Hook(HookUpload, func(_ *CmsCtx, _ Upload, allow *UploadAllow) error {
		allow.Extensions("png")
		allow.MaxBytes(1024)
		return nil
	})
	site.Hook(HookUpload, func(_ *CmsCtx, _ Upload, allow *UploadAllow) error {
		allow.MaxBytes(4096) // 抬高无效
		return nil
	})
	site.Setup()
	token := memberToken(t, site)

	small := append(bytes.Repeat(sniffSamples[".png"], 30), make([]byte, 512-len(sniffSamples[".png"])*30)...)
	if got := uploadFile(t, site, token, "small.png", small, nil); got.Code != http.StatusOK {
		t.Fatalf("512 字节该通过: %d %q (%d)", got.Code, got.Body.String(), len(small))
	}
	big := append(bytes.Repeat(sniffSamples[".png"], 100), make([]byte, 2048-len(sniffSamples[".png"])*100)...)
	if got := uploadFile(t, site, token, "big.png", big, nil); got.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("2048 字节该被站点定的 1 KiB 拦住: %d %q (%d)", got.Code, got.Body.String(), len(big))
	}
}

// 任何 handler 拒绝 ⇒ 整体拒绝（即使另一个 handler 放行）。
func TestUploadPolicyDenyWins(t *testing.T) {
	site := newPolicySite(t)
	site.Hook(HookUpload, func(_ *CmsCtx, _ Upload, allow *UploadAllow) error {
		allow.Extensions("png")
		return nil
	})
	site.Hook(HookUpload, func(_ *CmsCtx, _ Upload, _ *UploadAllow) error {
		return Forbidden("插件不允许")
	})
	site.Setup()
	got := uploadFile(t, site, memberToken(t, site), "a.png", sniffSamples[".png"], nil)
	if got.Code != http.StatusForbidden {
		t.Fatalf("任何一个 handler 拒绝就该拒: %d %q", got.Code, got.Body.String())
	}
	if files := uploadTree(t, site); len(files) != 0 {
		t.Fatalf("被拒的不该落盘: %v", files)
	}
}

// 两个 handler 的扩展名交集为空 ⇒ 配置错（响亮, 而不是"上传全 422"这种怪状态）。
func TestUploadPolicyEmptyIntersection(t *testing.T) {
	site := newPolicySite(t)
	site.Hook(HookUpload, func(_ *CmsCtx, _ Upload, allow *UploadAllow) error {
		allow.Extensions("png")
		return nil
	})
	site.Hook(HookUpload, func(_ *CmsCtx, _ Upload, allow *UploadAllow) error {
		allow.Extensions("pdf")
		return nil
	})
	site.Setup()
	got := uploadFile(t, site, memberToken(t, site), "a.png", sniffSamples[".png"], nil)
	if got.Code != http.StatusInternalServerError {
		t.Fatalf("交集为空该报配置错: %d %q", got.Code, got.Body.String())
	}
}

// 两个 handler 抢 Dir ⇒ 配置错（不静默让后跑的赢）。
func TestUploadPolicyDirConflict(t *testing.T) {
	site := newPolicySite(t)
	site.Hook(HookUpload, func(_ *CmsCtx, _ Upload, allow *UploadAllow) error {
		allow.Dir("member/1")
		return nil
	})
	site.Hook(HookUpload, func(_ *CmsCtx, _ Upload, allow *UploadAllow) error {
		allow.Dir("member/2")
		return nil
	})
	site.Setup()
	got := uploadFile(t, site, memberToken(t, site), "a.png", sniffSamples[".png"], nil)
	if got.Code != http.StatusInternalServerError {
		t.Fatalf("抢 Dir 该报配置错: %d %q", got.Code, got.Body.String())
	}
	// 设成同一个值则没问题
	same := newPolicySite(t)
	same.Hook(HookUpload, func(_ *CmsCtx, _ Upload, allow *UploadAllow) error {
		allow.Dir("member/1")
		return nil
	})
	same.Hook(HookUpload, func(_ *CmsCtx, _ Upload, allow *UploadAllow) error {
		allow.Dir("member/1")
		return nil
	})
	same.Setup()
	if got := uploadFile(t, same, memberToken(t, same), "a.png", sniffSamples[".png"], nil); got.Code != http.StatusOK {
		t.Fatalf("同值不该冲突: %d %q", got.Code, got.Body.String())
	}
}

// 规则能看到嗅探出来的真实内容类型（不是客户端说的）。
func TestUploadRuleSeesSniffedType(t *testing.T) {
	site := newPolicySite(t)
	var seen []Upload
	site.Hook(HookUpload, func(_ *CmsCtx, up Upload, allow *UploadAllow) error {
		seen = append(seen, up)
		allow.Extensions("png", "pdf")
		return nil
	})
	site.Setup()
	token := memberToken(t, site)
	uploadFile(t, site, token, "名字骗人.png", sniffSamples[".pdf"], nil) // 内容其实是 pdf
	if len(seen) != 1 {
		t.Fatalf("规则该跑一次: %#v", seen)
	}
	if seen[0].Ext != ".png" || seen[0].Mime != "application/pdf" {
		t.Fatalf("规则该看到 扩展名=%q 嗅探=%q（不是客户端说的）", seen[0].Ext, seen[0].Mime)
	}
	if seen[0].Name != "名字骗人.png" {
		t.Fatalf("原名该给规则看（审计用）: %q", seen[0].Name)
	}
}

// cmsNodeID 从令牌反查节点 id（测试里改自己节点的字段用）。
func cmsNodeID(t *testing.T, site *Site, token string) int64 {
	t.Helper()
	session, err := site.Engine().ValidSession(token)
	if err != nil || session == nil {
		t.Fatalf("会话无效: %v", err)
	}
	return session.NodeID
}
