import { useEffect, useState } from 'react';

export interface KnowledgeEntity {
  id: string;
  name: string;
  ticker?: string;
  entity_type: string;
  country?: string;
  sector?: string;
  recent_news_ids?: string[];
}

export interface KnowledgeRelationship {
  id: string;
  from_entity_id: string;
  to_entity_id: string;
  relationship_type: string;
  strength: number;
  confidence?: number;
}

interface KnowledgeGraphResponse {
  entities?: KnowledgeEntity[];
  relationships?: KnowledgeRelationship[];
}

export function useKnowledgeGraph() {
  const [entities, setEntities] = useState<KnowledgeEntity[]>([]);
  const [relationships, setRelationships] = useState<KnowledgeRelationship[]>([]);

  useEffect(() => {
    const fetchGraph = () => {
      fetch('/api/knowledge/graph?watchlist_only=true&limit=120')
        .then(r => { if (!r.ok) throw new Error(r.statusText); return r.json(); })
        .then((d: KnowledgeGraphResponse) => {
          setEntities(d.entities || []);
          setRelationships(d.relationships || []);
        })
        .catch(() => {
          setEntities([]);
          setRelationships([]);
        });
    };

    fetchGraph();
    const interval = setInterval(fetchGraph, 60_000);
    return () => clearInterval(interval);
  }, []);

  return { entities, relationships };
}
