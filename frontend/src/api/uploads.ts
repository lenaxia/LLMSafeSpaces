import { api } from "./client";

// UploadResponse mirrors agentd's FileUploadResponse as forwarded by the
// API upload route (Epic 68 US-68.2): POST /workspaces/:id/uploads with a
// single multipart part named "file" → 201 {path, name, size}.
export interface UploadResponse {
  path: string;
  name: string;
  size: number;
}

export const uploadsApi = {
  upload: (workspaceId: string, file: File): Promise<UploadResponse> => {
    const form = new FormData();
    form.append("file", file);
    return api.upload<UploadResponse>(`/workspaces/${workspaceId}/uploads`, form);
  },
};
