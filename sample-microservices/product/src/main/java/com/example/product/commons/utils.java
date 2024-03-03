package com.example.product.commons;

import java.io.File;
import java.io.IOException;
import java.util.HashMap;
import java.util.Map;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.ResponseEntity;
import org.springframework.web.client.RestTemplate;

import com.example.product.models.Response;
import com.fasterxml.jackson.databind.ObjectMapper;

public class utils {
    @Autowired
    private static RestTemplate restTemplate=new RestTemplate();
   
//function to load the contract
    public static com.example.product.models.Contract loadContract(String consumer, String provider) {
        // Create an ObjectMapper to read the class from the file
        ObjectMapper objectMapper = new ObjectMapper();
        

        com.example.product.models.Contract contract = null;
        try{
            String workingDirectory = System.getProperty("user.dir");
            System.out.println("Current working directory: " + workingDirectory);
            File jsonFile = new File("/home/ahmed/Desktop/GSOC/Keploy/Issues/issue1541/8085_8084.json");

            // Check if the file exists before attempting to read
            if (jsonFile.exists()) {
                // Read the content of the file and deserialize it into Contract class
                contract = objectMapper.readValue(jsonFile, com.example.product.models.Contract.class);
                System.out.println("Contract loaded successfully from: " + jsonFile.getAbsolutePath());
            } else {
                System.out.println("File not found: " + jsonFile.getAbsolutePath());
            }
        
        }
        catch (IOException e) {
            // Handle the exception according to your needs
           e.printStackTrace();
        }
        return contract;
    }
    public static Boolean verify(String port){
        com.example.product.models.Contract contract = loadContract("",port);
        // get the interactions
        if(contract==null || ! contract.getProvider().getName().equals(port)){
            return false;
        }
        // //make a map that takes string and string  
        // Map<String, String> contractRequest = new HashMap<>();

        // // Loop over the interactions
        // for (com.example.product.models.Interaction interaction : contract.getInteractions()) {
            
        //     contractRequest.put(interaction.getRequest().getMethod().toString(), interaction.getRequest().getURL());
        // }
        String expectedBody="hello";
        ResponseEntity<String> testResponse= makeReq(contract.getInteractions().get(0).getRequest().getMethod().getValue(), "http://localhost:8084/products/");
        if(testResponse.getBody().equals(expectedBody)){
            return true;
        }
        return false;
    }
    public static ResponseEntity<String> makeReq(String method,String url){
        if(method.equals("get")){
           return  restTemplate.getForEntity(url, String.class);
        }
        return null;
    }

}
