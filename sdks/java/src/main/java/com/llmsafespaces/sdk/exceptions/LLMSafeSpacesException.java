package com.llmsafespaces.sdk.exceptions;

public class LLMSafeSpacesException extends RuntimeException {
    private final int statusCode;

    public LLMSafeSpacesException(String message, int statusCode) {
        super(message);
        this.statusCode = statusCode;
    }

    public int getStatusCode() { return statusCode; }
}
