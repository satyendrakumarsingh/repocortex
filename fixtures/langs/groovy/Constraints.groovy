package org.grails.validation

import groovy.transform.CompileStatic
import groovy.util.logging.Slf4j

import java.lang.annotation.Retention

/**
 * Default implementation of the constrained property contract.
 */
@Slf4j
@CompileStatic
@SuppressWarnings(['unused', 'GroovyUnusedDeclaration'])
class DefaultConstrainedProperty implements ConstrainedProperty, Cloneable {

    public static final String DEFAULT_MESSAGE = 'default.invalid'
    private static final Map<String, String> CONSTRAINT_ALIASES = [size: 'sizeConstraint']

    protected Class<?> owningClass
    protected String propertyName
    private boolean display = true

    DefaultConstrainedProperty(Class<?> owningClass, String propertyName, Class<?> propertyType, Map<String, Object> constraints) {
        this.owningClass = owningClass
        this.propertyName = propertyName
    }

    String getPropertyName() { propertyName }

    Class<?> getOwningClass() { owningClass }

    boolean isDisplay() {
        display
    }

    void setDisplay(boolean display) {
        this.display = display
    }

    @Override
    void applyConstraint(String constraintName, Object constrainingValue) {
        log.debug "applying ${constraintName}"
    }

    @Override
    boolean hasAppliedConstraint(String constraintName) {
        false
    }

    static Object defaultValueFor(Class type) {
        type.isPrimitive() ? 0 : null
    }

    private static writeStackToFile(File f, Helper h) {
        f.text = h.dump()
    }

    def validate(Object target, Object propertyValue, Errors errors) {
        propertyValue
    }
}
