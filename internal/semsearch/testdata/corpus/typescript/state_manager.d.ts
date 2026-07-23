export interface SessionSnapshot {
  sessionToken: string;
  activePanel: string;
}

export declare class StateManager {
  constructor(initialState: SessionSnapshot);
  restoreSessionToken(snapshot: SessionSnapshot): string;
  getActivePanel(): string;
}
