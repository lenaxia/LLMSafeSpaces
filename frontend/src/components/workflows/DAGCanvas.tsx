import { useCallback, useState } from "react";
import {
  ReactFlow,
  ReactFlowProvider,
  Background,
  Controls,
  MiniMap,
  addEdge,
  useNodesState,
  useEdgesState,
  type Connection,
  type Edge,
  type Node,
  BackgroundVariant,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";

import { nodeTypes } from "./WorkflowNode";
import { NodeEditPanel } from "./NodeEditPanel";
import {
  type FlowNode,
  type NodeType,
  type WorkflowSpec,
  specToFlow,
  flowToSpec,
  makeNodeId,
  NODE_STYLES,
} from "./dagTypes";

const DEFAULT_NODE_DATA: Record<NodeType, Record<string, unknown>> = {
  script: { language: "python", handler: "def handler(input):\n    return {'result': 'ok'}" },
  agent: { prompt: "Process: {{.input}}", session: "ephemeral" },
  http: { method: "GET", url: "" },
  condition: { conditions: [{ id: "match", expression: "input.field == 'value'" }] },
};

interface DAGCanvasProps {
  spec: WorkflowSpec;
  onSpecChange: (spec: WorkflowSpec) => void;
}

function DAGCanvasInner({ spec, onSpecChange }: DAGCanvasProps) {
  const { nodes: initNodes, edges: initEdges } = specToFlow(spec);
  const [nodes, setNodes, onNodesChange] = useNodesState(initNodes as Node[]);
  const [edges, setEdges, onEdgesChange] = useEdgesState(initEdges);
  const [selectedNodeId, setSelectedNodeId] = useState<string | null>(null);

  const selectedNode = nodes.find((n) => n.id === selectedNodeId) as FlowNode | undefined;

  const syncToParent = useCallback((n: Node[], e: Edge[]) => {
    onSpecChange(flowToSpec(n as FlowNode[], e));
  }, [onSpecChange]);

  const onConnect = useCallback((conn: Connection) => {
    setEdges((eds) => {
      const next = addEdge({ ...conn, type: "smoothstep" }, eds);
      syncToParent(nodes, next);
      return next;
    });
  }, [setEdges, nodes, syncToParent]);

  const onNodeDragStop = useCallback(() => {
    syncToParent(nodes, edges);
  }, [nodes, edges, syncToParent]);

  const handleNodeClick = useCallback((_evt: unknown, node: Node) => {
    setSelectedNodeId(node.id);
  }, []);

  const handleNodeChange = useCallback((id: string, data: Record<string, unknown>) => {
    setNodes((nds) => {
      const next = nds.map((n) =>
        n.id === id ? { ...n, data: { ...n.data, ...data } } : n
      );
      syncToParent(next, edges);
      return next;
    });
  }, [setNodes, edges, syncToParent]);

  const handleNodeDelete = useCallback((id: string) => {
    setNodes((nds) => {
      const next = nds.filter((n) => n.id !== id);
      const nextEdges = edges.filter((e) => e.source !== id && e.target !== id);
      syncToParent(next, nextEdges);
      return next;
    });
    setEdges((eds) => eds.filter((e) => e.source !== id && e.target !== id));
  }, [setNodes, setEdges, edges, syncToParent]);

  const addNode = useCallback((type: NodeType) => {
    const id = makeNodeId();
    const newNode: Node = {
      id,
      type,
      position: {
        x: 200 + Math.random() * 200,
        y: 100 + nodes.length * 120,
      },
      data: { ...DEFAULT_NODE_DATA[type], label: `${NODE_STYLES[type].label} ${nodes.length + 1}` },
    };
    setNodes((nds) => {
      const next = [...nds, newNode];
      syncToParent(next, edges);
      return next;
    });
  }, [nodes.length, setNodes, edges, syncToParent]);

  const onEdgesChangeSync = useCallback((params: any) => {
    onEdgesChange(params);
    setTimeout(() => syncToParent(nodes, edges), 0);
  }, [onEdgesChange, nodes, edges, syncToParent]);

  return (
    <div className="flex h-full flex-1 overflow-hidden">
      <div className="flex flex-1 flex-col">
        <div className="flex gap-1 border-b border-border p-2">
          {(Object.keys(NODE_STYLES) as NodeType[]).map((t) => {
            const s = NODE_STYLES[t];
            return (
              <button
                key={t}
                onClick={() => addNode(t)}
                className="flex items-center gap-1 rounded-md border border-border px-2 py-1 text-xs hover:bg-accent"
                style={{ borderColor: `${s.color}55` }}
              >
                <span
                  className="inline-block h-2 w-2 rounded-full"
                  style={{ backgroundColor: s.color }}
                />
                {s.label}
              </button>
            );
          })}
        </div>
        <div className="flex-1">
          <ReactFlow
            nodes={nodes}
            edges={edges}
            onNodesChange={onNodesChange}
            onEdgesChange={onEdgesChangeSync}
            onConnect={onConnect}
            onNodeClick={handleNodeClick}
            onNodeDragStop={onNodeDragStop}
            nodeTypes={nodeTypes}
            fitView
            className="bg-background"
            defaultEdgeOptions={{ type: "smoothstep" }}
          >
            <Background variant={BackgroundVariant.Dots} gap={20} size={1} className="bg-background" />
            <Controls />
            <MiniMap
              nodeColor={(n) => {
                const s = NODE_STYLES[n.type as NodeType];
                return s?.color || "#6b7280";
              }}
              className="bg-background"
            />
          </ReactFlow>
        </div>
      </div>
      {selectedNode && (
        <NodeEditPanel
          node={selectedNode}
          onClose={() => setSelectedNodeId(null)}
          onChange={handleNodeChange}
          onDelete={handleNodeDelete}
        />
      )}
    </div>
  );
}

export function DAGCanvas(props: DAGCanvasProps) {
  return (
    <ReactFlowProvider>
      <DAGCanvasInner {...props} />
    </ReactFlowProvider>
  );
}
