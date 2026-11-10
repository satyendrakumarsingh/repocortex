package com.acme.payments;

import java.util.HashMap;
import java.util.List;
import java.util.Map;

/**
 * The negative fixture. It saves, it finds, it deletes, and none of it touches
 * a database: `save`, `findByName` and `delete` are ordinary Java method names,
 * and there is no JPA annotation, no persistence import and no EntityManager.
 */
public class PlainService {

    private final Map<String, String> cache = new HashMap<>();

    public void save(String key, String value) {
        cache.put(key, value);
    }

    public String findByName(String name) {
        return cache.get(name);
    }

    public void deleteByName(String name) {
        cache.remove(name);
    }

    public List<String> query(String prefix) {
        return List.of(prefix);
    }
}
