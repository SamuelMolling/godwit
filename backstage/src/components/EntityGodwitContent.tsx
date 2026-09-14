import { MissingAnnotationEmptyState, useEntity } from '@backstage/plugin-catalog-react';
import { GODWIT_TARGETS_ANNOTATION, getGodwitTargets } from '../annotation';
import { GodwitTargetsView } from './GodwitTargetsView';

export interface EntityGodwitContentProps {
  maxRuns?: number;
}

export const EntityGodwitContent = ({ maxRuns }: EntityGodwitContentProps) => {
  const { entity } = useEntity();
  const targets = getGodwitTargets(entity);
  if (targets.length === 0) {
    return <MissingAnnotationEmptyState annotation={GODWIT_TARGETS_ANNOTATION} />;
  }
  return <GodwitTargetsView targets={targets} maxRuns={maxRuns} />;
};
