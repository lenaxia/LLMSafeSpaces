import { api } from "./client";

export interface PlatformInfo {
  api: string;
  controller: string;
  frontend: string;
  relayRouter: string;
  baseRuntime: string;
}

export const platformInfoApi = {
  get: () => api.get<PlatformInfo>("/admin/platform-info"),
};
