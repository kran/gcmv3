<template>
    <!-- 初始化加载 -->
    <div v-if="phase === 'loading'" class="panel-init-loading">
        <div class="panel-init-spinner"></div>
        <div class="panel-init-text">GCM Admin</div>
    </div>

    <!-- 登录 -->
    <div v-else-if="phase === 'login'" class="panel-login-wrapper">
        <div class="panel-login-card">
            <div class="panel-login-logo">
                <span>GCM</span>
            </div>
            <div class="panel-login-subtitle">{{ siteName }} · 内容管理</div>
            <el-form @submit.prevent="doLogin" class="panel-login-form">
                <el-select v-if="loginRealms.length > 1" v-model="loginForm.realm" placeholder="登录渠道" size="large">
                    <el-option v-for="r in loginRealms" :key="r.realm" :label="r.realm" :value="r.realm" />
                </el-select>
                <el-input v-model="loginForm.identifier" placeholder="账号" size="large">
                    <template #prefix><el-icon><User /></el-icon></template>
                </el-input>
                <el-input v-model="loginForm.password" type="password" placeholder="密码" size="large"
                    show-password @keyup.enter="doLogin">
                    <template #prefix><el-icon><Lock /></el-icon></template>
                </el-input>
                <el-alert v-if="loginForm.error" :title="loginForm.error" type="error"
                    :closable="false" show-icon style="margin-bottom:4px;" />
                <el-button type="primary" :loading="loginForm.loading" size="large" style="width:100%"
                    @click="doLogin"><el-icon><User /></el-icon>登 录</el-button>
            </el-form>
        </div>
    </div>

    <!-- 主布局: Jira 骨架 (顶部横栏贯穿 + 下方侧栏/内容) -->
    <div v-else class="panel-shell">
        <header class="panel-topbar">
            <div class="topbar-brand">
                <img src="/admin/ui/logo.svg" class="topbar-logo-img" alt="gcm">
                <span class="topbar-brand-name">GCM</span>
            </div>
            <nav class="topbar-menu">
                <!-- 内容管理（第一个普通菜单 — 类型树/扩展插后边） -->
                <a v-for="item in mainBefore" :key="item.key" class="topbar-menu-item"
                   :class="{ active: route.name === item.route }"
                   @click.prevent="router.push({ name: item.route, params: item.params })">
                    <el-icon :size="15"><component :is="item.icon" /></el-icon>
                    <span>{{ item.label }}</span>
                </a>
                <el-dropdown v-if="panelMenu.length" trigger="hover" :show-timeout="0" :hide-timeout="0">
                    <a class="topbar-menu-item" :class="{ active: isPanelActive }">
                        <el-icon :size="15"><Grid /></el-icon>
                        <span>扩展</span>
                    </a>
                    <template #dropdown>
                        <el-dropdown-menu>
                            <el-dropdown-item v-for="item in panelMenu" :key="item.key"
                                @click="router.push({ name: item.route, params: item.params })">
                                {{ item.label }}
                            </el-dropdown-item>
                        </el-dropdown-menu>
                    </template>
                </el-dropdown>
                <!-- 其余普通菜单（站点配置/账号...） -->
                <a v-for="item in mainAfter" :key="item.key" class="topbar-menu-item"
                   :class="{ active: route.name === item.route }"
                   @click.prevent="router.push({ name: item.route, params: item.params })">
                    <el-icon :size="15"><component :is="item.icon" /></el-icon>
                    <span>{{ item.label }}</span>
                </a>
            </nav>
            <div class="topbar-right">
                <button class="topbar-icon-btn" @click="globalQ && globalSearch()"><el-icon><Search /></el-icon></button>
                <el-dropdown>
                    <button class="topbar-icon-btn"><el-icon><User /></el-icon></button>
                    <template #dropdown>
                        <el-dropdown-menu>
                            <el-dropdown-item @click="doLogout">退出登录</el-dropdown-item>
                        </el-dropdown-menu>
                    </template>
                </el-dropdown>
            </div>
        </header>

        <div class="panel-body">
            <div class="panel-main">
                <div class="panel-content">
                    <div class="page-title">{{ pageTitle }}</div>
                    <div class="content-card">
                        <!-- 同一路由不同 params（如 /tree/category → /tree/region）必须重建页面实例。 -->
                        <router-view :key="routeViewKey" />
                    </div>
                </div>
            </div>
        </div>
    </div>
</template>
<script>
import { ref, reactive, computed, onMounted, provide } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import $api from '$api'

export default {
    setup() {
        var route = useRoute()
        var router = useRouter()
        var Panel = window.Panel
        var menuData = ref(window.AppConfig.menu) // ref: 插件面板动态 push 后 UI 刷新
        var defaultPage = window.AppConfig.defaultPage
        // 固定菜单（内置）与动态菜单（tree/面板）— 动态项由 section 标记
        var mainMenu = computed(function () { return menuData.value.filter(function (m) { return !m.section }) })
        // 内容管理（第一个普通菜单）单独 — 类型树/扩展插它后边
        var mainBefore = computed(function () { return mainMenu.value.slice(0, 1) })
        var mainAfter = computed(function () { return mainMenu.value.slice(1) })
        // 分组菜单（TokenHub 风格）— 静态项按 group 归组; 动态项（tree/panel）归"管理"
        var menuGroups = computed(function () {
            var order = ['平台', '内容', '管理']
            var seen = {}
            menuData.value.forEach(function (m) {
                var g = m.group || '管理'
                if (!seen[g]) { seen[g] = { name: g, open: true, items: [] } }
                seen[g].items.push(m)
            })
            var groups = order.filter(function (g) { return seen[g] }).map(function (g) { return seen[g] })
            var rest = Object.keys(seen).filter(function (g) { return order.indexOf(g) < 0 }).map(function (g) { return seen[g] })
            return groups.concat(rest)
        })
        function globalSearch() {
            if (!globalQ.value) return
            router.push({ name: 'nodes', query: { q: globalQ.value } })
        }

        var phase = ref('loading')
        var user = ref(null)
        provide('user', user)

        var loginForm = reactive({ realm: '', identifier: '', password: '', loading: false, error: '' })
        // 可登录的**类型**从服务端拿（realm 清单）—— 页面不写死任何类型名
        var loginRealms = ref([])

        var siteName = computed(function () { return user.value?.site || '' })

        var routeViewKey = computed(function () {
            return String(route.name || '') + ':' + JSON.stringify(route.params || {})
        })

        var pageTitle = computed(function () {
            var name = route.name
            var list = menuData.value
            for (var i = 0; i < list.length; i++) {
                var item = list[i]
                if (item.route !== name) continue
                var params = item.params || {}
                var matches = Object.keys(params).every(function (key) {
                    return String(params[key]) === String(route.params[key])
                })
                if (matches) return item.label
            }
            return name || '首页'
        })

        // 防止 401 触发登出时, 登出请求自身再 401 造成递归
        var _loggingOut = false
        function doLogout() {
            if (_loggingOut) return
            _loggingOut = true
            $api.logout().catch(function () {}).finally(function () { _loggingOut = false })
            user.value = null
            phase.value = 'login'
            loadLoginRealms()   // 登录页的"登录类型"要重新拉（否则登出后没法再登录）
            router.push('/')
        }

        async function loadLoginRealms() {
            try {
                var res = await $api.loginRealms()
                loginRealms.value = res.realms || []
                if (!loginForm.realm && loginRealms.value.length) {
                    // 默认渠道优先（AuthRealm.Default）; 没有就取第一个
                    var preferred = loginRealms.value.find(function (r) { return r['default'] })
                    loginForm.realm = (preferred || loginRealms.value[0]).name
                }
            } catch (e) { console.error('[panel] loadLoginRealms failed:', e) }
        }

        async function checkAuth() {
            try {
                user.value = await $api.me()
                phase.value = 'app'
                console.log('[app] checkAuth ok, phase=app, route=', route.name, route.path)
                loadPanels()   // 站点面板: 动态注册路由 + 菜单
            } catch (_) {
                console.log('[app] checkAuth failed → login')
                phase.value = 'login'
                loadLoginRealms()
            }
        }

        // 站点面板: /admin/panels 返回 [{path, title, vue}] — 动态 addRoute + 菜单
        async function loadPanels() {
            try {
                var res = await $api.get('/admin/panels')
                ;(res.items || []).forEach(function (p) {
                    if (!p.vue || !p.path) return
                    var name = 'panel' + p.path.replace(/[^a-zA-Z0-9]/g, '')
                    var exists = menuData.value.some(function (m) { return m.key === name })
                    if (exists) return // 去重（checkAuth 与 doLogin 都可能调到）
                    router.addRoute({ name: name, path: p.path, component: Vue.defineAsyncComponent({
                        loader: function () { return Panel.loadComponent(p.vue) },
                        loadingComponent: { template: '<div style="padding:40px;text-align:center;color:#999;">加载中...</div>' },
                        delay: 100,
                    }) })
                    menuData.value.push({ key: name, label: p.title, icon: 'Grid', route: name, section: 'panel' })
                })
                // 刷新后 hash 残留动态路由页: 初次渲染时未注册导致失配 — 重新匹配
                if (route.name === undefined && route.path !== '/') {
                    router.replace(route.fullPath)
                }
            } catch (e) { console.error('[panel] loadPanels failed:', e) }
        }

        async function doLogin() {
            loginForm.loading = true
            loginForm.error = ''
            var realm = loginForm.realm || (loginRealms.value[0] && loginRealms.value[0].realm)
            if (!realm) { loginForm.error = '没有可登录的类型'; loginForm.loading = false; return }
            try {
                // 走该类型自己的登录端点（会员用微信、员工用用户名密码…）⇒ 同一个 session
                await $api.login(realm, loginForm.identifier, loginForm.password)
                user.value = await $api.me()
                if (!user.value || !user.value.actor || !hasAdminRole(user.value.actor)) {
                    // 登录成功但没有后台角色 —— 说清楚, 不是"密码错误"
                    user.value = null
                    loginForm.error = '该账号没有后台权限（需要 owner 或 admin 角色）'
                    loginForm.loading = false
                    return
                }
                phase.value = 'app'
                loadPanels()   // 站点面板: 动态注册路由 + 菜单
                if (defaultPage) router.push({ name: defaultPage })
            } catch (e) {
                loginForm.error = (e && e.message) ? e.message : '登录失败'
            }
            loginForm.loading = false
        }

        // 后台权限判定与后端同义：owner / admin 角色（后台 = 前台 + 权限）
        function hasAdminRole(actor) {
            var roles = actor.roles || []
            return roles.indexOf('owner') >= 0 || roles.indexOf('admin') >= 0
        }

        onMounted(function () {
            console.log('[app] mounted, 初始路由:', route.name, route.path, '| hash:', location.hash)
            Panel.onError(function (err) { if (err.status === 401) doLogout() })
            checkAuth()
        })
        // 路由变化日志（router-view 渲染谁）
        router.afterEach(function (to) {
            console.log('[app] route →', to.name, to.path)
        })

        return {
            phase: phase, user: user, siteName: siteName, pageTitle: pageTitle, routeViewKey: routeViewKey,
            menuData: menuData, menuGroups: menuGroups, globalQ: globalQ, globalSearch: globalSearch,
            mainMenu: mainMenu, mainBefore: mainBefore, mainAfter: mainAfter, panelMenu: panelMenu,
            isPanelActive: isPanelActive,
            loginForm: loginForm, loginRealms: loginRealms,
            route: route, router: router,
            doLogin: doLogin, doLogout: doLogout,
        }
    },
}
</script>
