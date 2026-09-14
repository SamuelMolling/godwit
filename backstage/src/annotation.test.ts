import { Entity } from '@backstage/catalog-model';
import { GODWIT_TARGETS_ANNOTATION, getGodwitTargets, isGodwitAvailable, parseGodwitTargets } from './annotation';

const entity = (annotations?: Record<string, string>): Entity => ({
  apiVersion: 'backstage.io/v1alpha1',
  kind: 'Component',
  metadata: { name: 'orders', annotations },
});

describe('parseGodwitTargets', () => {
  it('splits on commas, trims, drops empties and keeps the first of each name in order', () => {
    expect(parseGodwitTargets(' orders-staging, orders-production ,,orders-staging ')).toEqual([
      'orders-staging',
      'orders-production',
    ]);
  });

  it('reads a single name and an absent value', () => {
    expect(parseGodwitTargets('orders')).toEqual(['orders']);
    expect(parseGodwitTargets(undefined)).toEqual([]);
    expect(parseGodwitTargets(' , ')).toEqual([]);
  });
});

describe('isGodwitAvailable', () => {
  it('is true only when the annotation names a target', () => {
    expect(isGodwitAvailable(entity({ [GODWIT_TARGETS_ANNOTATION]: 'orders' }))).toBe(true);
    expect(isGodwitAvailable(entity({ [GODWIT_TARGETS_ANNOTATION]: ' ' }))).toBe(false);
    expect(isGodwitAvailable(entity())).toBe(false);
    expect(getGodwitTargets(entity({ other: 'x' }))).toEqual([]);
  });
});
