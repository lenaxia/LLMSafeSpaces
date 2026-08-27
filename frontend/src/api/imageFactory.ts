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
  updatesAvailable?: BaseUpdates;
}

// Base-update pill payload (#928). Absent = fresh. Per ruling #29 the
// base is the version axis for apt-track packages, so a base migration
// is THE version migration for system packages.
export interface BaseUpdates {
  kind: "version_bump" | "base_migration";
  currentBaseName: string;
  currentBaseVersion: string;
  latestBaseVersion?: string;
  defaultBaseName?: string;
  defaultBaseVersion?: string;
}

// Content-address lookup for a schematic hash: the selection it names
// plus every base version built under it (newest first). Any
// authenticated user may resolve any hash — images are platform-wide
// artifacts and a hash reveals only catalog extension IDs.
export interface HashResolution {
  hash: string;
  selection: string[];
  baseName: string;
  versions: string[];
}

export const imageFactoryApi = {
  getCatalog: () => api.get<Catalog>("/image-factory/catalog"),

  listConfigs: async () => {
    const r = await api.get<{ configs?: Config[] } | Config[]>("/image-factory/configs");
    return Array.isArray(r) ? r : (r.configs ?? []);
  },

  getConfig: (hash: string) => api.get<Config>(`/image-factory/configs/${hash}`),

  resolveHash: (hash: string) => api.get<HashResolution>(`/image-factory/resolve/${hash}`),

  createConfig: (req: {
    name: string;
    selection: string[];
    baseName: string;
    baseVersion?: string;
  }) => api.post<Config>("/image-factory/configs", req),

  createOrgConfig: (orgId: string, req: {
    name: string;
    selection: string[];
    baseName: string;
    baseVersion?: string;
  }) => api.post<Config>(`/orgs/${orgId}/image-factory/configs`, req),

  createPlatformConfig: (req: {
    name: string;
    selection: string[];
    baseName: string;
    baseVersion?: string;
  }) => api.post<Config>("/admin/image-factory/configs", req),

  deleteConfig: (hash: string) =>
    api.delete<void>(`/image-factory/configs/${hash}`),

  renameConfig: (hash: string, name: string) =>
    api.patch<Config>(`/image-factory/configs/${hash}`, { name }),
};
