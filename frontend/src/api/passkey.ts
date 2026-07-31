import { api } from "./client";
import type {
  PublicKeyCredentialCreationOptionsJSON,
  PublicKeyCredentialRequestOptionsJSON,
} from "@simplewebauthn/browser";

export interface PasskeyBeginResponse {
  options: PublicKeyCredentialCreationOptionsJSON;
  sessionToken: string;
}

export interface PasskeyLoginBeginResponse {
  options: PublicKeyCredentialRequestOptionsJSON;
  sessionToken: string;
}

export interface PasskeyRegisterFinishResponse {
  token: string;
  recoveryCodes?: string[];
}

export interface PasskeyLoginFinishResponse {
  token: string;
  user?: import("./types").User;
}

export interface PasskeyRecoverResponse {
  token: string;
  user?: import("./types").User;
  mustEnrollPasskey: boolean;
}

export interface PasskeyListItem {
  id: string;
  name?: string;
  createdAt: string;
  lastUsedAt?: string;
}

export const passkeyApi = {
  registerBegin: (email: string, name?: string) =>
    api.post<PasskeyBeginResponse>("/auth/passkey/register/begin", { email, name }),
  registerFinish: (sessionToken: string, email: string, response: unknown, name?: string) =>
    api.post<PasskeyRegisterFinishResponse>("/auth/passkey/register/finish", {
      sessionToken,
      email,
      response,
      name,
    }),
  loginBegin: (email: string) =>
    api.post<PasskeyLoginBeginResponse>("/auth/passkey/login/begin", { email }),
  loginFinish: (sessionToken: string, email: string, response: unknown) =>
    api.post<PasskeyLoginFinishResponse>("/auth/passkey/login/finish", {
      sessionToken,
      email,
      response,
    }),
  recover: (email: string, code: string) =>
    api.post<PasskeyRecoverResponse>("/auth/passkey/recover", { email, code }),
  listPasskeys: () =>
    api.get<{ passkeys: PasskeyListItem[] }>("/account/passkeys"),
  deletePasskey: (id: string) =>
    api.delete<{ deleted: boolean }>(`/account/passkeys/${id}`),
  regenerateRecoveryCodes: () =>
    api.post<{ recoveryCodes: string[] }>("/account/passkeys/recovery-codes/regenerate"),
};
