import { Entity } from '@backstage/catalog-model';

export const GODWIT_TARGETS_ANNOTATION = 'godwit/targets';

export function parseGodwitTargets(value: string | undefined): string[] {
  const seen = new Set<string>();
  for (const part of (value ?? '').split(',')) {
    const name = part.trim();
    if (name) {
      seen.add(name);
    }
  }
  return [...seen];
}

export function getGodwitTargets(entity: Entity): string[] {
  return parseGodwitTargets(entity.metadata.annotations?.[GODWIT_TARGETS_ANNOTATION]);
}

export function isGodwitAvailable(entity: Entity): boolean {
  return getGodwitTargets(entity).length > 0;
}
