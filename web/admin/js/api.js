/* gcm 业务端点 (数据层): 只做 URL 与参数映射, 请求细节在 Panel。 */
window.$api = {
    get: function (url, params) { return Panel.get(url, params) },
    post: function (url, body) { return Panel.post(url, body) },
    put: function (url, body) { return Panel.put(url, body) },
    del: function (url) { return Panel.del(url) },

    // 认证 —— 后台**没有自己的登录端点**（后台 = 前台 + 权限）:
    //   登录页先列"能登录的类型"(realm), 再调该类型自己的 auth 插件端点。
    me:          function () { return Panel.get('/admin/me') },
    types:       function () { return Panel.get('/admin/types') },
    loginRealms: function () { return Panel.get('/admin/login/realms') },
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
    // ref 字段经 fields 提交, 引擎落边; expand=* = 展开自动声明的引用一层（列表要显示引用名）。
    nodes:      function (type, query) { return Panel.get('/api/nodes/' + type, query) },
    node:       function (type, id) { return Panel.get('/api/nodes/' + type + '/' + id, { expand: '*' }) },
    createNode: function (type, n) { return Panel.post('/api/nodes/' + type, n) },
    updateNode: function (type, id, n) { return Panel.put('/api/nodes/' + type + '/' + id, n) },
    deleteNode: function (type, id) { return Panel.del('/api/nodes/' + type + '/' + id) },
    // parent = 用哪个字段当父（服务端要求显式给：内核不推导"哪个自引用字段是父"）
    tree:       function (type, parent) { return Panel.get('/api/tree/' + type, { expand: '*', parent: parent }) },

    // 实体检索 (引用编辑器) / 入边引用
    search:  function (query) { return Panel.get('/api/search/nodes', query) },
    inbound: function (id, query) { return Panel.get('/api/inbound/' + id, query) },

    // refLabel: 节点显示名（任何消费端统一）— 后台列表/引用选择器用。
    // 优先级: display → 类型定义字段序第一个非空字符串标量
    // （关系节点兜底）→ expand 引用合成 → #id。
    refLabel: function (n, def) {
        if (!n) { console.log('[refLabel] null node'); return '#?' }
        if (n.display) return n.display
        if (def) {
            for (const f of def.fields || []) {
                const v = (n.fields || {})[f.name]
                if (typeof v === 'string' && v.trim()) return v
            }
        }
        const parts = []
        for (const v of Object.values(n.expand || {})) {
            const arr = Array.isArray(v) ? v : (v ? [v] : [])
            for (const m of arr) if (m && m.display) parts.push(m.display)
        }
        if (parts.length) return parts.join('·')
        console.log('[refLabel] 兜底 #id:', { id: n.id, type: n.type, display: n.display,
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
