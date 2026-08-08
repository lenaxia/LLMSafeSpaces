import { memo } from "react";
import { Handle, Position, type NodeProps } from "@xyflow/react";
import { Code2, Bot, Globe, GitBranch } from "lucide-react";
import { NODE_STYLES, type NodeType } from "./dagTypes";

const ICONS = { Code2, Bot, Globe, GitBranch } as const;

interface WorkflowNodeData {
  label?: string;
  language?: string;
  agent?: string;
  url?: string;
  conditions?: { id: string; expression: string }[];
  [key: string]: unknown;
}

function WorkflowNodeBase({ id, data, selected, type }: NodeProps & { type: NodeType }) {
  const style = NODE_STYLES[type];
  const Icon = ICONS[style.icon as keyof typeof ICONS] || Code2;
  const nodeData = data as WorkflowNodeData;
  const conditionBranches = type === "condition" ? (nodeData.conditions || []) : [];

  let subtitle = "";
  if (type === "script") subtitle = nodeData.language || "python";
  else if (type === "agent") subtitle = nodeData.agent || "default";
  else if (type === "http") subtitle = (nodeData.url || "").slice(0, 40) || "GET";
  else if (type === "condition") subtitle = `${conditionBranches.length} branches`;

  return (
    <div
      className="relative rounded-lg border-2 px-3 py-2 min-w-[180px] max-w-[260px] transition-shadow"
      style={{
        borderColor: selected ? style.color : `${style.color}55`,
        backgroundColor: style.bg,
        boxShadow: selected ? `0 0 0 2px ${style.color}33` : "none",
      }}
    >
      <Handle type="target" position={Position.Top} style={{ background: style.color }} />

      <div className="flex items-center gap-2">
        <Icon className="h-4 w-4 shrink-0" style={{ color: style.color }} />
        <div className="min-w-0 flex-1">
          <div className="truncate text-sm font-medium text-white">
            {nodeData.label || id}
          </div>
          <div className="truncate text-xs text-white/60">{subtitle}</div>
        </div>
        <span className="rounded px-1.5 py-0.5 text-[9px] font-bold uppercase" style={{ color: style.color, backgroundColor: `${style.color}22` }}>
          {style.label}
        </span>
      </div>

      <Handle type="source" position={Position.Bottom} style={{ background: style.color }} />

      {type === "condition" && conditionBranches.length > 0 && (
        <div className="mt-1 flex flex-col gap-1">
          {conditionBranches.map((c) => (
            <div key={c.id} className="relative">
              <div className="flex items-center gap-1 text-[10px] text-white/70">
                <span className="rounded bg-white/10 px-1">{c.id}</span>
                <span className="truncate">{c.expression}</span>
              </div>
              <Handle
                type="source"
                position={Position.Right}
                id={c.id}
                style={{ background: style.color, top: "50%", right: -4 }}
              />
            </div>
          ))}
          <div className="relative">
            <div className="flex items-center gap-1 text-[10px] text-white/50">
              <span className="rounded bg-white/10 px-1">otherwise</span>
              <span>default</span>
            </div>
            <Handle
              type="source"
              position={Position.Right}
              id="otherwise"
              style={{ background: style.color, top: "50%", right: -4 }}
            />
          </div>
        </div>
      )}
    </div>
  );
}

export const ScriptNode = memo((props: NodeProps) => <WorkflowNodeBase {...props} type="script" />);
export const AgentNode = memo((props: NodeProps) => <WorkflowNodeBase {...props} type="agent" />);
export const HTTPNode = memo((props: NodeProps) => <WorkflowNodeBase {...props} type="http" />);
export const ConditionNode = memo((props: NodeProps) => <WorkflowNodeBase {...props} type="condition" />);

export const nodeTypes = {
  script: ScriptNode,
  agent: AgentNode,
  http: HTTPNode,
  condition: ConditionNode,
};
