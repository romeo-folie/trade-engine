export function resetConfigPreservingStrategies<
  T extends { strategies: S },
  S,
>(defaults: T, current: T): T {
  return {
    ...defaults,
    strategies: current.strategies,
  };
}
