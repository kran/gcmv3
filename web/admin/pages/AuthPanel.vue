<!-- 登录凭据面板 —— 只管"这个节点有哪些登录凭据、怎么改"。
     自己的数据、自己的动作、自己的样式; 主窗体只决定**放不放它**。

     只对 owner 出现（能改别人密码 = 能冒充别人, 与 roles 同级）。服务端独立校验
     owner-only —— 这里藏起来是省事, 不是安全边界。
     拿不到（非 owner / 未登录 / 非 auth 类型）就整块不显示: 它是附加面板,
     失败不该弹全局提示（authMethods 走静默请求）。 -->
<template>
    <div v-if="isOwner" class="auth-block">
        <div class="fr-label">
            <span>登录凭据</span>
            <span class="fr-kind">owner 专用</span>
        </div>
        <div v-if="methods.length" class="auth-list">
            <div v-for="item in methods" :key="item.method + ':' + item.identifier" class="auth-row">
                <span class="auth-method">{{ item.method }}</span>
                <span class="auth-identifier">{{ item.identifier }}</span>
                <el-button link size="small" type="danger" @click="remove(item.method)">解绑</el-button>
            </div>
        </div>
        <div v-else class="auth-empty">还没有登录凭据（这个人登不进来）</div>
        <div class="auth-form">
            <el-select v-model="form.method" size="small" style="width:118px;">
                <el-option v-for="name in passwordMethods" :key="name" :label="name" :value="name" />
            </el-select>
            <el-input v-model="form.identifier" size="small" placeholder="邮箱 / 手机号 / 用户名"
                      style="width:220px;" />
            <el-input v-model="form.secret" size="small" type="password" show-password
                      placeholder="新口令（至少 8 位）" style="width:180px;" />
            <el-button size="small" type="primary" @click="save">设置口令</el-button>
        </div>
        <p class="fr-hint">
            设置后该账号**所有登录态会被踢掉**（口令在服务端 bcrypt 存储, 这里不回显）。
        </p>
    </div>
</template>
<script>
// 主窗体传入的是**类型 + 节点 id**（不是整个节点）—— 面板不该关心节点的其它字段。
export default {
    name: 'AuthPanel',
    props: {
        type: { type: String, required: true },
        nodeId: { type: Number, required: true },
    },
    data() {
        return {
            isOwner: false,       // 判不出来就当不是（fail-closed: 宁可不显示）
            methods: [],          // 已有凭据（方式 + 标识, 永无哈希）
            passwordMethods: [],  // 类型声明过、框架能设口令的方式
            form: { method: '', identifier: '', secret: '' },
        }
    },
    mounted() { this.load() },
    methods: {
        // load 判身份（owner 才显示）+ 拉凭据。两层都 fail-closed: 任何一步失败就
        // 保持不显示 —— 附加面板不该因为它自己把人挡在编辑之外。
        load() {
            window.$api.me().then((me) => {
                var roles = (me && me.actor && me.actor.roles) || []
                this.isOwner = roles.indexOf('owner') >= 0
                if (!this.isOwner) return
                window.$api.authMethods(this.type, this.nodeId).then((res) => {
                    this.methods = res.methods || []
                    this.passwordMethods = res.password_methods || []
                    if (!this.form.method && this.passwordMethods.length) {
                        this.form.method = this.passwordMethods[0]
                    }
                }).catch(() => {})
            }).catch(() => {})
        },
        // save 设置/重置口令（服务端改完会踢掉该账号所有会话）。
        save() {
            if (!this.form.method || !this.form.identifier || !this.form.secret) {
                ElementPlus.ElMessage.warning('方式 / 标识 / 新口令都要填')
                return
            }
            window.$api.authSet(this.type, this.nodeId, {
                method: this.form.method,
                identifier: this.form.identifier.trim(),
                secret: this.form.secret,
            }).then(() => {
                ElementPlus.ElMessage.success('口令已设置（该账号的登录态已失效）')
                this.form.secret = ''
                this.load()
            }).catch((err) => { ElementPlus.ElMessage.error(err.message || '设置失败') })
        },
        remove(method) {
            window.$api.authRemove(this.type, this.nodeId, method).then(() => {
                ElementPlus.ElMessage.success('已解绑')
                this.load()
            }).catch((err) => { ElementPlus.ElMessage.error(err.message || '解绑失败') })
        },
    },
}
</script>
<style>
.auth-block { margin-top: 16px; padding: 10px 12px; border: 1px solid #f0d9d9; border-radius: 6px; background: #fffaf9; }
.auth-list { margin-bottom: 8px; }
.auth-row { display: flex; align-items: center; gap: 10px; padding: 2px 0; font-size: 13px; }
.auth-method { min-width: 72px; color: #999; }
.auth-identifier { flex: 1; color: #333; word-break: break-all; }
.auth-empty { margin-bottom: 8px; color: #a19f9d; font-size: 12px; font-style: italic; }
.auth-form { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; }
.fr-kind { font-weight: 400; color: #aaa; font-size: 11px; margin-left: 6px; }
.fr-label { font-size: 13px; font-weight: 600; color: #444; margin-bottom: 4px; }
</style>
