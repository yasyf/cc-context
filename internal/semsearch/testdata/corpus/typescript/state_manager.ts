export interface SessionSnapshot {
  sessionToken: string;
  activePanel: string;
}

export class StateManager {
  private currentState: SessionSnapshot;

  constructor(initialState: SessionSnapshot) {
    this.currentState = initialState;
  }

  restoreSessionToken(snapshot: SessionSnapshot): string {
    this.currentState = snapshot;
    return this.currentState.sessionToken;
  }

  getActivePanel(): string {
    return this.currentState.activePanel;
  }
}
