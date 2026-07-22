use std::collections::HashMap;

pub struct CacheCoordinator {
    records: HashMap<String, String>,
}

impl CacheCoordinator {
    pub fn new() -> Self {
        Self { records: HashMap::new() }
    }

    pub fn refresh_cache_entry(&mut self, cache_key: &str, fresh_value: &str) -> String {
        self.records.insert(cache_key.to_owned(), fresh_value.to_owned());
        self.records.get(cache_key).cloned().unwrap()
    }

    pub fn cached_record(&self, cache_key: &str) -> Option<&String> {
        self.records.get(cache_key)
    }
}
