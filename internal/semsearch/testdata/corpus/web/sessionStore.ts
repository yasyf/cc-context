export interface StoredSession {
  accessToken: string;
  refreshToken: string;
  expiresAt: number;
}

export class SessionStore {
  private sessionsByUser = new Map<string, StoredSession>();

  saveSession(userId: string, session: StoredSession): void {
    this.sessionsByUser.set(userId, session);
  }

  loadSession(userId: string): StoredSession | undefined {
    return this.sessionsByUser.get(userId);
  }

  refreshSessionToken(userId: string, accessToken: string, expiresAt: number): StoredSession {
    const currentSession = this.sessionsByUser.get(userId);
    if (!currentSession) {
      throw new Error(`missing session for ${userId}`);
    }
    const refreshedSession = { ...currentSession, accessToken, expiresAt };
    this.sessionsByUser.set(userId, refreshedSession);
    return refreshedSession;
  }

  removeExpiredSessions(now: number): string[] {
    const removedUserIds: string[] = [];
    for (const [userId, session] of this.sessionsByUser) {
      if (session.expiresAt <= now) {
        this.sessionsByUser.delete(userId);
        removedUserIds.push(userId);
      }
    }
    return removedUserIds.sort();
  }
}
