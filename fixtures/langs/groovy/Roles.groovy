package org.acme.security

import groovy.transform.CompileStatic

enum Role implements Comparable<Role> {
    ADMIN('A'), USER('U'), GUEST('G')

    final String code

    Role(String code) {
        this.code = code
    }

    boolean canWrite() {
        this != GUEST
    }

    static Role fromCode(String code) {
        values().find { it.code == code }
    }
}

@CompileStatic
trait Auditable<T> implements Serializable {
    Date created = new Date()

    abstract T snapshot()

    String describe() {
        "audit ${created}"
    }
}

interface Visitor<R> {
    R visit(Node n)

    void done()
}

@interface Marker {
    String value() default ''
}

class Registry {
    private final Map<String, Closure> handlers = [:]

    def register(String name, Closure c) {
        handlers[name] = c
    }

    Closure lookup(String name) { handlers[name] }
}
