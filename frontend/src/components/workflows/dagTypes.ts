import type { Node, Edge } from "@xyflow/react";

export type NodeType = "script" | "agent" | "http" | "condition";

export interface WorkflowSpec {
  nodes: SpecNode[];
  edges: SpecEdge[];
}

export interface SpecNode {
  id: string;
  type: NodeType;
  position?: { x: number; y: number };
  data: Record<string, unknown>;
  maxAttempts?: number;
  timeout?: string;
}

export interface SpecEdge {
  source: string;
  target: string;
  sourceHandle?: string;
}

export interface ScriptNodeData {
  language: "python" | "node";
  handler: string;
  env?: Record<string, string>;
}

export interface AgentNodeData {
  agent?: string;
  prompt: string;
  system?: string;
  outputSchema?: unknown;
  enforceStructuredOutput?: boolean;
  session?: "ephemeral" | "new" | "existing";
  sessionId?: string;
}

export interface HTTPNodeData {
  method?: string;
  url: string;
  headers?: Record<string, string>;
  body?: string;
  timeout?: string;
}

export interface ConditionNodeData {
  conditions: { id: string; expression: string }[];
}

export interface FlowNode extends Node {
  data: Record<string, unknown>;
}

export function specToFlow(spec: WorkflowSpec): { nodes: FlowNode[]; edges: Edge[] } {
  const nodes: FlowNode[] = (spec.nodes || []).map((n, i) => ({
    id: n.id,
    type: n.type,
    position: n.position || { x: 100 + i * 250, y: 100 },
    data: {
      ...n.data,
      maxAttempts: n.maxAttempts,
      timeout: n.timeout,
    },
  }));

  const edges: Edge[] = (spec.edges || []).map((e) => ({
    id: `${e.source}${e.sourceHandle ? `__${e.sourceHandle}` : ""}__${e.target}`,
    source: e.source,
    target: e.target,
    sourceHandle: e.sourceHandle || undefined,
    type: "smoothstep",
    label: e.sourceHandle && e.sourceHandle !== "otherwise" ? e.sourceHandle : undefined,
  }));

  return { nodes, edges };
}

export function flowToSpec(nodes: FlowNode[], edges: Edge[]): WorkflowSpec {
  return {
    nodes: nodes.map((n) => {
      const data = { ...n.data };
      delete data.maxAttempts;
      delete data.timeout;
      delete data.label;
      return {
        id: n.id,
        type: n.type as NodeType,
        position: n.position,
        data,
        maxAttempts: n.data.maxAttempts as number | undefined,
        timeout: n.data.timeout as string | undefined,
      };
    }),
    edges: edges.map((e) => ({
      source: e.source,
      target: e.target,
      sourceHandle: e.sourceHandle || undefined,
    })),
  };
}

export function parseSpec(specYaml: string): WorkflowSpec {
  const trimmed = specYaml.trim();
  if (!trimmed) return { nodes: [], edges: [] };
  try {
    const parsed = JSON.parse(trimmed);
    return {
      nodes: parsed.nodes || [],
      edges: parsed.edges || [],
    };
  } catch {
    return { nodes: [], edges: [] };
  }
}

export function serializeSpec(spec: WorkflowSpec): string {
  return JSON.stringify(spec, null, 2);
}

export const NODE_STYLES: Record<NodeType, { color: string; bg: string; icon: string; label: string }> = {
  script: { color: "#3b82f6", bg: "#1e3a5f", icon: "Code2", label: "Script" },
  agent: { color: "#8b5cf6", bg: "#3b1e5f", icon: "Bot", label: "Agent" },
  http: { color: "#10b981", bg: "#0f3d2e", icon: "Globe", label: "HTTP" },
  condition: { color: "#f59e0b", bg: "#4a3010", icon: "GitBranch", label: "Condition" },
};

export function makeNodeId(): string {
  return `node-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`;
}
