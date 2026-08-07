import { useState, useEffect, useCallback } from 'react';
import { workflowApi, type Workflow } from '@/api/workflows';

export function WorkflowsTab() {
  const [workflows, setWorkflows] = useState<Workflow[]>([]);
  const [loading, setLoading] = useState(true);
  const [selected, setSelected] = useState<Workflow | null>(null);
  const [showCreate, setShowCreate] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const data = await workflowApi.list();
      setWorkflows(data);
    } catch (e) {
      console.error('Failed to load workflows:', e);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { load(); }, [load]);

  if (loading) return <div className="p-4 text-muted-foreground">Loading workflows...</div>;

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold">Workflows</h2>
        <button
          className="px-3 py-1.5 text-sm bg-primary text-primary-foreground rounded-md hover:bg-primary/90"
          onClick={() => setShowCreate(true)}
        >
          New Workflow
        </button>
      </div>

      {workflows.length === 0 && !showCreate && (
        <div className="text-center py-8 text-muted-foreground">
          No workflows yet. Create one to automate tasks.
        </div>
      )}

      {showCreate && (
        <WorkflowEditor
          onSave={async (name, spec, status) => {
            try {
              await workflowApi.create({ name, specYaml: spec, status });
              setShowCreate(false);
              load();
            } catch (e) {
              alert('Failed to create workflow: ' + e);
            }
          }}
          onCancel={() => setShowCreate(false)}
        />
      )}

      <div className="space-y-2">
        {workflows.map((wf) => (
          <div
            key={wf.id}
            className="border rounded-lg p-3 hover:bg-accent cursor-pointer"
            onClick={() => setSelected(wf)}
          >
            <div className="flex items-center justify-between">
              <div>
                <span className="font-medium">{wf.name}</span>
                <span className={`ml-2 text-xs px-1.5 py-0.5 rounded ${
                  wf.status === 'active' ? 'bg-green-100 text-green-700' :
                  wf.status === 'draft' ? 'bg-gray-100 text-gray-600' :
                  'bg-yellow-100 text-yellow-700'
                }`}>
                  {wf.status}
                </span>
              </div>
              <span className="text-xs text-muted-foreground">
                {new Date(wf.updatedAt).toLocaleDateString()}
              </span>
            </div>
            {wf.description && (
              <p className="text-sm text-muted-foreground mt-1">{wf.description}</p>
            )}
          </div>
        ))}
      </div>

      {selected && (
        <WorkflowDetail
          workflow={selected}
          onClose={() => setSelected(null)}
          onChanged={load}
        />
      )}
    </div>
  );
}

function WorkflowEditor({ onSave, onCancel, initial }: {
  onSave: (name: string, spec: string, status: string) => void;
  onCancel: () => void;
  initial?: Workflow;
}) {
  const [name, setName] = useState(initial?.name || '');
  const [spec, setSpec] = useState(initial?.specYaml || defaultSpec);
  const [status, setStatus] = useState(initial?.status || 'draft');

  return (
    <div className="border rounded-lg p-4 space-y-3">
      <div className="flex gap-2">
        <input
          className="flex-1 px-2 py-1 border rounded text-sm"
          placeholder="Workflow name"
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
        <select
          className="px-2 py-1 border rounded text-sm"
          value={status}
          onChange={(e) => setStatus(e.target.value)}
        >
          <option value="draft">Draft</option>
          <option value="active">Active</option>
          <option value="archived">Archived</option>
        </select>
      </div>
      <textarea
        className="w-full h-64 px-2 py-1 border rounded text-sm font-mono"
        placeholder="Workflow DAG spec (JSON)"
        value={spec}
        onChange={(e) => setSpec(e.target.value)}
        spellCheck={false}
      />
      <div className="flex gap-2">
        <button
          className="px-3 py-1 text-sm bg-primary text-primary-foreground rounded"
          onClick={() => onSave(name, spec, status)}
          disabled={!name || !spec}
        >
          {initial ? 'Update' : 'Create'}
        </button>
        <button
          className="px-3 py-1 text-sm border rounded"
          onClick={onCancel}
        >
          Cancel
        </button>
      </div>
    </div>
  );
}

function WorkflowDetail({ workflow, onClose, onChanged }: {
  workflow: Workflow;
  onClose: () => void;
  onChanged: () => void;
}) {
  return (
    <div className="fixed inset-0 bg-black/50 flex items-center justify-center z-50" onClick={onClose}>
      <div className="bg-background rounded-lg p-6 max-w-2xl w-full max-h-[80vh] overflow-auto" onClick={(e) => e.stopPropagation()}>
        <div className="flex justify-between items-start mb-4">
          <div>
            <h3 className="text-lg font-semibold">{workflow.name}</h3>
            <p className="text-sm text-muted-foreground">{workflow.slug}</p>
          </div>
          <button onClick={onClose} className="text-muted-foreground hover:text-foreground">✕</button>
        </div>
        <pre className="bg-muted p-3 rounded text-sm font-mono overflow-auto">
          {workflow.specYaml}
        </pre>
        <div className="flex gap-2 mt-4">
          <button
            className="px-3 py-1 text-sm bg-primary text-primary-foreground rounded"
            onClick={async () => {
              try {
                await workflowApi.run(workflow.id);
                alert('Workflow started!');
              } catch (e) {
                alert('Failed to start: ' + e);
              }
            }}
          >
            Run Now
          </button>
          <button
            className="px-3 py-1 text-sm border rounded text-destructive"
            onClick={async () => {
              if (confirm('Delete this workflow?')) {
                await workflowApi.delete(workflow.id);
                onChanged();
                onClose();
              }
            }}
          >
            Delete
          </button>
        </div>
      </div>
    </div>
  );
}

const defaultSpec = JSON.stringify({
  nodes: [
    { id: "start", type: "script", data: { language: "python", handler: "def handler(input):\n    return {\"result\": \"hello\"}" } }
  ],
  edges: []
}, null, 2);
