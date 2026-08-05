import { api } from "./client";

export interface Base {
  name: string;
  version: string;
  image: string;
  tag?: string;
  digest?: string;
  isDefault: boolean;
}

export interface FileSpec {
  path: string;
  mode?: string;
}

export interface Extension {
  id: string;
  type: "apt" | "mise" | "file";
  value: string;
  fileSpec?: FileSpec;
  supportedBases: string[];
  retired: boolean;
  reviewRequested: boolean;
  description?: string;
}

export interface KnownFailure {
  selectionHash: string;
  selection: string[];
  baseName: string;
  explanation?: string;
  detectedAt: string;
  retriable: boolean;
}

export interface Catalog {
  architectures: string[];
  bases: Base[];
  extensions: Extension[];
  knownFailures: KnownFailure[];
}

export interface Config {
  id: string;
  hash: string;
  name: string;
  selection: string[];
  resolvedValues: Record<string, { type: string; value: string; fileSpec?: FileSpec }>;
  baseName: string;
  baseVersion: string;
  scope: "member" | "org" | "platform";
  ownerId?: string;
  orgId?: string;
  status: "building" | "ready" | "rejected";
}

export const imageFactoryApi = {
  getCatalog: () => api.get<Catalog>("/image-factory/catalog"),

  listConfigs: () => api.get<{ configs: Config[] }>("/image-factory/configs"),

  getConfig: (hash: string) => api.get<Config>(`/image-factory/configs/${hash}`),

  createConfig: (req: {
    name: string;
    selection: string[];
    baseName: string;
    baseVersion?: string;
  }) => api.post<Config>("/image-factory/configs", req),

  deleteConfig: (hash: string) =>
    api.delete<void>(`/image-factory/configs/${hash}`),

  renameConfig: (hash: string, name: string) =>
    api.patch<Config>(`/image-factory/configs/${hash}`, { name }),
};
