<template>
  <button v-if="user" type="button" class="block max-w-64 truncate text-left text-xs text-primary-600 dark:text-primary-400"
    :title="[user.source_id, user.user_id, user.token_id, user.request_id].filter(Boolean).join(' / ')"
    @click.stop="$emit('select', user)">
    {{ user.source_name || user.source_id }} · {{ user.username || user.user_id }} #{{ user.user_id }}
  </button>
</template>
<script setup lang="ts">
import type { SourceUser } from '@/types'
defineProps<{ user?: SourceUser | null }>()
defineEmits<{ select: [user: SourceUser] }>()
</script>
