import { SessionStore, StoredSession } from "../web/sessionStore";

async function loginExample(username: string, password: string): Promise<StoredSession> {
  const response = await fetch("/api/session", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ username, password }),
  });
  if (!response.ok) {
    throw new Error(`login failed with status ${response.status}`);
  }
  return (await response.json()) as StoredSession;
}

export async function createExampleSession(username: string, password: string): Promise<SessionStore> {
  const store = new SessionStore();
  const session = await loginExample(username, password);
  store.saveSession(username, session);
  return store;
}
