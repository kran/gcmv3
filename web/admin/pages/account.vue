<template>
    <div style="max-width:420px;">
        <h3 style="margin-top:0;">账号</h3>
        <el-card>
            <template #header>
                <span>设置密码</span>
            </template>
            <el-form label-width="80px">
                <el-form-item label="账号">
                    <el-input v-model="form.identifier" />
                </el-form-item>
                <el-form-item label="新密码">
                    <el-input v-model="form.new_password" type="password" show-password />
                </el-form-item>
                <el-form-item label="确认新密">
                    <el-input v-model="form.confirm" type="password" show-password />
                </el-form-item>
                <el-form-item>
                    <el-button type="primary" :loading="saving" @click="doChange"><el-icon><Key /></el-icon>修改密码</el-button>
                </el-form-item>
            </el-form>
            <div style="color:#999;font-size:12px;line-height:1.8;">
                用户: {{ user?.username }} · 类型: {{ user?.actor?.realm }}<br>
                密码归认证插件管（后台不碰凭证）: 这里调 <code>/api/auth/&lt;类型&gt;/bind</code> 换一条认证方式。<br>
                新密码至少 8 位。
            </div>
        </el-card>
    </div>
</template>
<script>
import { reactive, ref, inject } from 'vue'
import $api from '$api'

export default {
    setup() {
        const user = inject('user')
        const form = reactive({
            // 账号默认取当前身份的用户名（也是登录标识; 换标识就改这里）
            identifier: (user.value && user.value.username) || '',
            new_password: '', confirm: '',
        })
        const saving = ref(false)

        async function doChange() {
            const realm = user.value && user.value.actor && user.value.actor.realm
            if (!realm) { ElMessage.error('当前身份不属于任何认证类型'); return }
            if (!form.identifier) { ElMessage.error('账号不能为空'); return }
            if (form.new_password.length < 8) { ElMessage.error('新密码至少 8 位'); return }
            if (form.new_password !== form.confirm) { ElMessage.error('两次输入不一致'); return }
            saving.value = true
            try {
                await $api.bindSecret(realm, form.identifier, form.new_password)
                ElMessage.success('密码已设置')
                form.new_password = form.confirm = ''
            } catch (_) {}
            finally { saving.value = false }
        }

        return { user, form, saving, doChange }
    }
}
</script>
