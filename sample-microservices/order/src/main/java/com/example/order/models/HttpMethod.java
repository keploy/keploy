package com.example.order.models;



public enum HttpMethod {
    CONNECT("connect"),
    DELETE("delete"),
    GET("get"),
    HEAD("head"),
    OPTIONS("options"), 
    POST("post"),
    PUT("put"),
    TRACE("trace");

    private final String value;

    HttpMethod(String value) {
        this.value = value;
    }

    public String getValue() {
        return value;
    }
}
