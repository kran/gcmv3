/* gcm 业务端点 (数据层): 只做 URL 与参数映射, 请求细节在 Panel。 */
window.$api = {
    get: function (url, params) { return Panel.get(url, params) },
    post: function (url, body) { return Panel.post(url, body) },
    put: function (url, body) { return Panel.put(url, body) },
    del: function (url) { return Panel.del(url) },

    // 认证 —— 后台**没有自己的登录端点**（后台 = 前台 + 权限）:
    //   登录页先列"能登录的类型"(realm), 再调该类型自己的 auth 插件端点。
    me:          function () { return Panel.get('/api/auth/me') },
    types:       function () { return Panel.get('/admin/types') },
    loginRealms: function () { return Panel.get('/api/auth/realms') },
    login:       function (realm, identifier, secret) {
        return Panel.post('/api/auth/' + realm + '/login', { method: 'username', identifier: identifier, secret: secret })
    },
    // 登出与类型无关（一个浏览器一个 session）
    logout:      function () { return Panel.post('/api/auth/logout') },
    // 改密 = 给当前节点换一条认证方式（凭证格式归 auth 插件, 内核不碰密码）
    bindSecret:  function (realm, identifier, secret) {
        return Panel.post('/api/auth/' + realm + '/bind', { method: 'username', identifier: identifier, secret: secret })
    },

    // 节点 —— 与前台/小程序**同一批端点**（后台只是身份不同: 前台 + 权限）。
    // ref 字段经 fields 提交, 引擎落边; 读入口按类型**自动展开一层**引用（不需要参数）。
    nodes:      function (type, query) { return Panel.get('/api/nodes/' + type, query) },
    node:       function (type, id) { return Panel.get('/api/nodes/' + type + '/' + id) },
    createNode: function (type, n) { return Panel.post('/api/nodes/' + type, n) },
    updateNode: function (type, id, n) { return Panel.put('/api/nodes/' + type + '/' + id, n) },
    deleteNode: function (type, id) { return Panel.del('/api/nodes/' + type + '/' + id) },
    // 树视图: 服务端一次把整棵树装好（不受 /api/nodes 的 size 上限影响;
    // 读规则照旧生效 —— 读不到的行不进树）。sort 同列表: `$sort,-$sort`。
    tree:       function (type, sort) { return Panel.get('/admin/tree/' + type, sort ? { sort: sort } : {}) },

    // 登录凭据（**只有 owner** —— 服务端独立校验, 这里只是面板）
    // 静默: 拿不到凭据（非 owner / 非 auth 类型 / 未登录）就是"面板不显示",
    // 不该弹全局错误提示
    authMethods: function (type, id) {
        return Panel.get('/admin/auth/' + type + '/' + id, null, { quiet: true })
    },
    authSet:     function (type, id, body) { return Panel.post('/admin/auth/' + type + '/' + id, body) },
    authRemove:  function (type, id, method) { return Panel.del('/admin/auth/' + type + '/' + id + '/' + method) },

    // refLabel: 节点显示名（任何消费端统一）— 后台列表/引用选择器用。
    // 优先级: 合成节点自带的 label（预置值回显）→ 类型 **admin.columns** 里第一个非空标量
    //        → 类型字段序里第一个非空字符串标量 → expand 引用合成 → #id。
    // （没有 display 这种系统列了: 显示什么完全由 types 声明决定。）
    refLabel: function (n, def) {
        if (!n) { console.log('[refLabel] null node'); return '#?' }
        if (n.label) return n.label
        // ① 类型**显式声明**的显示名字段（types.yaml 的 admin.display）—— 最可靠
        const declared = ((def || {}).admin || {}).display
        if (declared) {
            const value = (n.fields || {})[declared]
            if (typeof value === 'string' && value.trim()) return value
        }
        // ② 退到 admin.columns 里第一个非空标量（"猜" —— 声明了 display 就不该走到这）
        const cols = ((def || {}).admin || {}).columns || []
        for (const name of cols) {
            const v = (n.fields || {})[name]
            if (typeof v === 'string' && v.trim()) return v
        }
        if (def) {
            for (const f of def.fields || []) {
                const v = (n.fields || {})[f.name]
                if (typeof v === 'string' && v.trim()) return v
            }
        }
        const parts = []
        for (const v of Object.values(n.expand || {})) {
            const arr = Array.isArray(v) ? v : (v ? [v] : [])
            for (const m of arr) if (m && m.label) parts.push(m.label)
            else if (m && m.fields) parts.push(window.$api.refLabel(m, (window.$api._defs || {})[m.type]))
        }
        if (parts.length) return parts.join('·')
        console.log('[refLabel] 兜底 #id:', { id: n.id, type: n.type,
            hasDef: !!def, fields: n.fields, expandKeys: Object.keys(n.expand || {}) })
        return '#' + n.id
    },

    // 上传 (multipart; 不经 Panel 的 JSON 通道)
    upload: function (file) {
        var fd = new FormData()
        fd.append('file', file)
        return fetch('/api/upload', { method: 'POST', body: fd, credentials: 'same-origin' })
            .then(async function (resp) {
                var body = {}
                try { body = await resp.json() } catch (_) {}
                if (!resp.ok) {
                    var err = new Error(body.error || ('HTTP ' + resp.status))
                    err.status = resp.status
                    if (resp.status !== 401 && window.ElementPlus) ElementPlus.ElMessage.error(err.message)
                    throw err
                }
                return body
            })
    },
}
