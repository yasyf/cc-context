<script setup lang="ts">
import { computed, ref } from "vue";

type Account = {
  name: string;
  email: string;
  plan: "starter" | "team";
};

const props = defineProps<{ account: Account }>();
const emit = defineEmits<{ upgrade: [email: string] }>();
const detailsVisible = ref(false);

const planLabel = computed(() =>
  props.account.plan === "team" ? "Team plan" : "Starter plan",
);

function requestUpgrade() {
  emit("upgrade", props.account.email);
}
</script>

<template>
  <section class="account-panel">
    <header>
      <div>
        <h2>{{ account.name }}</h2>
        <p>{{ planLabel }}</p>
      </div>
      <button type="button" @click="detailsVisible = !detailsVisible">
        {{ detailsVisible ? "Hide details" : "Show details" }}
      </button>
    </header>

    <dl v-if="detailsVisible">
      <dt>Email</dt>
      <dd>{{ account.email }}</dd>
      <dt>Plan</dt>
      <dd>{{ account.plan }}</dd>
    </dl>

    <button v-if="account.plan === 'starter'" type="button" @click="requestUpgrade">
      Request upgrade
    </button>
  </section>
</template>

<style scoped>
.account-panel {
  display: grid;
  gap: 1rem;
  padding: 1.25rem;
  border: 1px solid #d6dae3;
  border-radius: 0.75rem;
}

.account-panel header {
  display: flex;
  align-items: start;
  justify-content: space-between;
}

.account-panel h2,
.account-panel p,
.account-panel dl {
  margin: 0;
}
</style>
