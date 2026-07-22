use crate::cache::CacheCoordinator;

pub fn refresh_legacy_cache(coordinator: &mut CacheCoordinator, key: &str, value: &str) -> String {
    coordinator.refresh_cache_entry(key, value)
}
