export type UserIdentifier = string;

export interface RefreshSessionRequest {
  userId: UserIdentifier;
  refreshToken: string;
}

export interface RefreshSessionResponse {
  accessToken: string;
  expiresAt: number;
}

export declare function refreshSessionToken(
  request: RefreshSessionRequest,
): Promise<RefreshSessionResponse>;
