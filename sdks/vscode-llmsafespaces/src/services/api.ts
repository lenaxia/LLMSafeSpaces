import * as vscode from "vscode";
import { LLMSafeSpaces, type WorkspaceListItem } from "@llmsafespaces/sdk";

export class ApiService {
  private client: LLMSafeSpaces | undefined;

  constructor(private context: vscode.ExtensionContext) {
    this.initClient();
  }

  isConfigured(): boolean {
    return !!vscode.workspace.getConfiguration("llmsafespaces").get<string>("apiUrl");
  }

  /** Re-read config and rebuild client. Called when settings change. */
  reinitialize(): void {
    this.initClient();
  }

  /** Clean up resources. */
  dispose(): void {
    this.client = undefined;
  }

  async configure(): Promise<void> {
    const currentUrl = vscode.workspace.getConfiguration("llmsafespaces").get<string>("apiUrl") || "";
    const url = await vscode.window.showInputBox({
      title: "LLMSafeSpaces API URL",
      prompt: "Enter the base URL of your LLMSafeSpaces instance",
      placeHolder: "https://llmsafespaces.example.com",
      value: currentUrl,
      validateInput: (v) => {
        if (!v) return "URL is required";
        try { new URL(v); return null; } catch { return "Invalid URL"; }
      },
    });
    if (!url) return;

    const apiKey = await vscode.window.showInputBox({
      title: "LLMSafeSpaces API Key",
      prompt: "Enter your API key (starts with lsp_)",
      password: true,
      validateInput: (v) => {
        if (!v) return "API key is required";
        if (!v.startsWith("lsp_")) return "API key must start with lsp_";
        return null;
      },
    });
    if (!apiKey) return;

    await vscode.workspace.getConfiguration("llmsafespaces").update("apiUrl", url, vscode.ConfigurationTarget.Global);
    await this.context.secrets.store("llmsafespaces.apiKey", apiKey);
    this.initClient();
    vscode.window.showInformationMessage("LLMSafeSpaces configured successfully.");
    vscode.commands.executeCommand("llmsafespaces.refresh");
  }

  async listWorkspaces(): Promise<WorkspaceListItem[]> {
    this.ensureClient();
    const result = await this.client!.workspaces.list();
    return result.items;
  }

  async createWorkspace(name: string, runtime: string): Promise<void> {
    this.ensureClient();
    await this.client!.workspaces.create({ name, runtime, storageSize: "10Gi" });
  }

  async suspendWorkspace(id: string): Promise<void> {
    this.ensureClient();
    await this.client!.workspaces.suspend(id);
  }

  async activateWorkspace(id: string): Promise<void> {
    this.ensureClient();
    await this.client!.workspaces.activate(id);
  }

  async deleteWorkspace(id: string): Promise<void> {
    this.ensureClient();
    await this.client!.workspaces.delete(id);
  }

  async getTerminalTicket(id: string): Promise<string> {
    this.ensureClient();
    const ticket = await this.client!.terminal.getTicket(id);
    return ticket.ticket;
  }

  async sendMessage(workspaceId: string, sessionId: string, content: string): Promise<string> {
    this.ensureClient();
    const resp = await this.client!.sessions.sendMessage(workspaceId, sessionId, content);
    // Contract Message (pkg/session): plain text is the derived view —
    // the text field, else concatenated text parts.
    return resp.text ?? (resp.parts ?? []).filter(p => p.type === "text").map(p => p.text ?? "").join("");
  }

  async ensureSession(workspaceId: string): Promise<string> {
    this.ensureClient();
    const resp = await this.client!.sessions.ensure(workspaceId);
    return resp.sessionId;
  }

  private ensureClient(): void {
    if (!this.client) {
      throw new Error("LLMSafeSpaces not configured. Run 'LLMSafeSpaces: Configure' from the command palette.");
    }
  }

  private async initClient(): Promise<void> {
    const url = vscode.workspace.getConfiguration("llmsafespaces").get<string>("apiUrl");
    if (!url) { this.client = undefined; return; }
    const apiKey = await this.context.secrets.get("llmsafespaces.apiKey");
    if (!apiKey) { this.client = undefined; return; }
    this.client = new LLMSafeSpaces({ baseUrl: url, apiKey, timeout: 120_000 });
  }
}
